package snapshot

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// seededStore opens an in-memory store, seeds the demo against assetRef, and
// returns it (closed on cleanup).
func seededStore(t *testing.T, assetRef string) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SeedDemo(context.Background(), assetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	return s
}

// rawRowsFromSection projects a wire.ScheduleSection's opaque row arrays back
// into a datamodel.RawRows bundle, so a test can round-trip the built section
// through datamodel.ValidateRows.
func rawRowsFromSection(sec wire.ScheduleSection) datamodel.RawRows {
	return datamodel.RawRows{
		Playlists:       sec.Playlists,
		Schedules:       sec.Schedules,
		ValidityWindows: sec.ValidityWindows,
		Dayparts:        sec.Dayparts,
		Fallbacks:       sec.Fallbacks,
		PresetBatches:   sec.PresetBatches,
	}
}

// TestBuildFromStoreOverSeededStore: BuildFromStore over a seeded store produces
// a snapshot whose schedule section round-trips datamodel.ValidateRows with zero
// errors, whose site_effective matches the site node, whose generation equals the
// store generation, and whose hash/signature invariant holds (REL-053/075).
func TestBuildFromStoreOverSeededStore(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)
	assetRef := signhash.ContentID(img)
	s := seededStore(t, assetRef)

	ds, err := s.DesiredState(context.Background())
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}

	snap, err := BuildFromStore(ds, img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}

	// Generation tracks the store's counter, not a hardcoded 1.
	if snap.Generation != ds.Generation {
		t.Errorf("Generation = %d, want the store generation %d", snap.Generation, ds.Generation)
	}
	if snap.Generation <= 1 {
		t.Errorf("Generation = %d, want the store's post-seed generation (> 1)", snap.Generation)
	}

	// The carried schedule section is a complete, referentially-sound data-model/1
	// row set.
	if _, errs := datamodel.ValidateRows(rawRowsFromSection(snap.Sections.Schedule)); len(errs) != 0 {
		t.Fatalf("built schedule section failed data-model validation: %+v", errs)
	}

	// site_effective from the site node (DAT-033), not box-local.
	se := snap.Sections.RevocationAndSite.SiteEffective
	if se != ds.SiteEffective {
		t.Errorf("site_effective = %+v, want the store-derived site node placement %+v", se, ds.SiteEffective)
	}
	if se.TZ != "America/Chicago" {
		t.Errorf("site_effective.TZ = %q, want America/Chicago (the site node's own tz)", se.TZ)
	}

	// The baseline screen_programs / edge_rules / pairing_grants shape matches Build:
	// one image screen-program, one demo edge rule, an empty (non-nil) grants slice.
	if len(snap.Sections.ScreenPrograms) != 1 || snap.Sections.ScreenPrograms[0].Display != "content" {
		t.Errorf("ScreenPrograms = %+v, want one display:content program", snap.Sections.ScreenPrograms)
	}
	if snap.Sections.ScreenPrograms[0].Content[0].AssetRef != assetRef {
		t.Errorf("screen program asset_ref = %q, want %q", snap.Sections.ScreenPrograms[0].Content[0].AssetRef, assetRef)
	}
	if len(snap.Sections.EdgeRules.Rules) != 1 {
		t.Errorf("EdgeRules.Rules = %d, want the 1 baseline demo rule", len(snap.Sections.EdgeRules.Rules))
	}
	if snap.Sections.PairingGrants == nil || len(snap.Sections.PairingGrants) != 0 {
		t.Errorf("PairingGrants = %#v, want a non-nil empty slice", snap.Sections.PairingGrants)
	}

	// The schedule section carries all seven arrays, none null (REL-060); the
	// unused ones marshal as [].
	raw, err := json.Marshal(snap.Sections.Schedule)
	if err != nil {
		t.Fatalf("marshal schedule section: %v", err)
	}
	var sched map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sched); err != nil {
		t.Fatalf("unmarshal schedule section: %v", err)
	}
	for _, k := range []string{"validity_windows", "fallbacks"} {
		if string(sched[k]) != "[]" {
			t.Errorf("schedule.%s = %s, want [] (empty, never null — REL-060)", k, sched[k])
		}
	}
	for _, k := range []string{"scope_nodes", "playlists", "schedules", "dayparts", "preset_batches"} {
		if string(sched[k]) == "[]" || string(sched[k]) == "null" {
			t.Errorf("schedule.%s = %s, want the seeded rows", k, sched[k])
		}
	}

	// hash + signature invariant (REL-053/075): recompute the hash over the sections
	// and verify the signature over {generation, hash}.
	recomputed, err := hashSections(snap.Sections)
	if err != nil {
		t.Fatalf("hashSections: %v", err)
	}
	if recomputed != snap.Hash {
		t.Errorf("recomputed hash %q != snapshot hash %q (REL-053)", recomputed, snap.Hash)
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
		t.Error("signature did not verify over the store-derived snapshot (REL-075)")
	}
}

// TestBuildFromStoreGenerationAdvances: after a store write bumps the generation,
// a rebuild produces a higher-generation snapshot carrying the new row — the seam
// that makes an authored edit change what the relay pulls.
func TestBuildFromStoreGenerationAdvances(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)
	assetRef := signhash.ContentID(img)
	s := seededStore(t, assetRef)
	ctx := context.Background()

	ds1, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState #1: %v", err)
	}
	snap1, err := BuildFromStore(ds1, img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("BuildFromStore #1: %v", err)
	}

	// Author a new playlist over the store (an api-shaped write): the generation
	// must advance.
	newPlaylist := datamodel.Playlist{
		ID: "01J8ZNEWPLAYLISTAUTHORED1", ScopeNode: "01J8Z4DEMOSCREENFIRSTPHOTN",
		Name: "Authored Playlist", Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: assetRef}},
	}
	pb, _ := json.Marshal(newPlaylist)
	if _, err := s.Create(ctx, store.KindPlaylist, pb); err != nil {
		t.Fatalf("author new playlist: %v", err)
	}

	ds2, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState #2: %v", err)
	}
	snap2, err := BuildFromStore(ds2, img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("BuildFromStore #2: %v", err)
	}

	if !(snap2.Generation > snap1.Generation) {
		t.Fatalf("generation did not advance across an authored write: %d -> %d", snap1.Generation, snap2.Generation)
	}
	if got, want := len(snap2.Sections.Schedule.Playlists), len(snap1.Sections.Schedule.Playlists)+1; got != want {
		t.Fatalf("playlists in rebuilt snapshot = %d, want %d (the new authored row carried)", got, want)
	}
	// The rebuild still validates.
	if _, errs := datamodel.ValidateRows(rawRowsFromSection(snap2.Sections.Schedule)); len(errs) != 0 {
		t.Fatalf("rebuilt schedule section failed validation: %+v", errs)
	}
	// Different generations bind different signed bytes (REL-075): snap1's signature
	// must not verify against snap2's {generation, hash}.
	canon2, err := generationHashCanonBytes(snap2.Generation, snap2.Hash)
	if err != nil {
		t.Fatalf("generationHashCanonBytes: %v", err)
	}
	sig1, err := wire.DecodeSignature(snap1.Signature)
	if err != nil {
		t.Fatalf("DecodeSignature: %v", err)
	}
	if signhash.Verify(id.SigningPub(), canon2, sig1) {
		t.Error("snap1's signature verified against snap2's generation/hash — signatures must bind their own generation")
	}
}

// TestBuildFromStoreEmitsContentOrigin asserts BuildFromStore, like Build,
// carries its contentBaseURL argument into revocation_and_site.content_origin
// (REL-061/066) over the store-derived path too — riding the same
// hash/signature invariant as every other section member (REL-053/075).
func TestBuildFromStoreEmitsContentOrigin(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)
	assetRef := signhash.ContentID(img)
	s := seededStore(t, assetRef)

	ds, err := s.DesiredState(context.Background())
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}

	snap, err := BuildFromStore(ds, img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}

	if got, want := snap.Sections.RevocationAndSite.ContentOrigin, "https://origin.example"; got != want {
		t.Errorf("RevocationAndSite.ContentOrigin = %q, want %q (REL-061/066)", got, want)
	}

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
		t.Error("signature did not verify with a populated content_origin field over the store-derived path (REL-075)")
	}
}

func TestBuildFromStoreRejectsNilIdentity(t *testing.T) {
	img := loadTestImage(t)
	s := seededStore(t, signhash.ContentID(img))
	ds, err := s.DesiredState(context.Background())
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	if _, err := BuildFromStore(ds, img, "https://origin.example", nil, nil); err == nil {
		t.Error("BuildFromStore(nil identity) succeeded, want an error")
	}
}
