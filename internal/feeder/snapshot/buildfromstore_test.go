package snapshot

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// seededStore opens an in-memory store, seeds the demo against assetRef, and
// returns it (closed on cleanup).
func seededStore(t *testing.T, assetRef string) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:", store.WallClockMs)
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
		Casts:           sec.Casts,
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

	snap, _, err := BuildFromStore(ds, "https://origin.example", id, contentInstant(t), nil)
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

	// screen_programs is derived: one entry per seeded SCREEN ROW, named by that
	// row's id (DAT-004a) — never by the screen-kind scope node it sits under —
	// and showing the seeded playlist's own asset inside the content daypart.
	if len(snap.Sections.ScreenPrograms) != 1 || snap.Sections.ScreenPrograms[0].Display != "content" {
		t.Fatalf("ScreenPrograms = %+v, want one display:content program", snap.Sections.ScreenPrograms)
	}
	if got := snap.Sections.ScreenPrograms[0].ScreenID; got != store.SeedScreenID {
		t.Errorf("screen_id = %q, want the seeded screen ROW id %q", got, store.SeedScreenID)
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
	snap1, _, err := BuildFromStore(ds1, "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore #1: %v", err)
	}

	// Author a new playlist over the store (an api-shaped write): the generation
	// must advance.
	newPlaylist := datamodel.Playlist{
		ID: "01J8ZNEWP1AY11STAVTH0RED10", ScopeNode: "01J8Z4DEM0SCREENF1RSTPH0TN",
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
	snap2, _, err := BuildFromStore(ds2, "https://origin.example", id, contentInstant(t), nil)
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

	snap, _, err := BuildFromStore(ds, "https://origin.example", id, contentInstant(t), nil)
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
	if _, _, err := BuildFromStore(ds, "https://origin.example", nil, contentInstant(t), nil); err == nil {
		t.Error("BuildFromStore(nil identity, nil) succeeded, want an error")
	}
}

// secondEdgeAutomationID/appAutomationID are fixture-ULID rule ids (no secrets)
// authored directly over the store — distinct from the seeded demo rule id
// (store.SeedDemo's automation row) — used to prove BuildFromStore's edge_rules
// section is derived from the store's edge-classified automations (REL-062),
// not a hardcoded constant.
const (
	secondEdgeAutomationID = "01J8ZB0EDGE0RV1E0BV11DFS01"
	appAutomationID        = "01J8ZB0APP00RV1E0BV11DFS01"
)

// secondEdgeAutomationBody is a second, well-formed edge rule (a state trigger
// on the same demo entity firing a device_command on it, RUL-002 edge) — an
// authored rule distinct from the seeded demo one. It states `enabled` because
// an automation authored without it is created DISABLED and would never reach
// edge_rules, which is the very carry this case asserts.
func secondEdgeAutomationBody() json.RawMessage {
	return json.RawMessage(`{"id":"` + secondEdgeAutomationID + `","enabled":true,"mode":"single",` +
		`"triggers":[{"type":"state","entity_id":"` + demoRuleEntityID + `","to":["off"]}],` +
		`"conditions":[],` +
		`"actions":[{"type":"device_command","entity_id":"` + demoRuleEntityID + `","command":"launch","params":{"channel":"second"}}]}`)
}

// appClassAutomationBody is a well-formed rule that compiles but classifies APP
// (a notify action is app-class unconditionally, RUL-210) — it must never ride
// edge_rules (REL-062).
func appClassAutomationBody() json.RawMessage {
	return json.RawMessage(`{"id":"` + appAutomationID + `","mode":"single",` +
		`"triggers":[{"type":"state","entity_id":"` + demoRuleEntityID + `","to":["on"]}],` +
		`"conditions":[],` +
		`"actions":[{"type":"notify","message":"hello"}]}`)
}

// carriedRuleIDs unmarshals the top-level "id" of each edge_rules.rules element.
func carriedRuleIDs(t *testing.T, rules []json.RawMessage) []string {
	t.Helper()
	ids := make([]string, len(rules))
	for i, r := range rules {
		var top struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(r, &top); err != nil {
			t.Fatalf("unmarshal carried edge rule %d: %v", i, err)
		}
		ids[i] = top.ID
	}
	return ids
}

// TestBuildFromStoreCarriesSeededDemoAsEdgeRule: BuildFromStore over a store
// seeded with the demo automation (store.SeedDemo) carries exactly that rule in
// edge_rules — derived from store.EdgeRuleBodies, not the former hardcoded
// demoEdgeRuleJSON constant (REL-062).
func TestBuildFromStoreCarriesSeededDemoAsEdgeRule(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)
	assetRef := signhash.ContentID(img)
	s := seededStore(t, assetRef)
	ctx := context.Background()

	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	snap, _, err := BuildFromStore(ds, "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}

	if got, want := len(snap.Sections.EdgeRules.Rules), 1; got != want {
		t.Fatalf("EdgeRules.Rules = %d, want %d (the store-seeded demo rule)", got, want)
	}
	if got := carriedRuleIDs(t, snap.Sections.EdgeRules.Rules); got[0] != demoRuleID {
		t.Errorf("carried edge rule id = %q, want the seeded demo rule id %q", got[0], demoRuleID)
	}
	if got := snap.Sections.EdgeRules.RulesMinorVersion; got != rulesMinorVersion {
		t.Errorf("EdgeRules.RulesMinorVersion = %q, want %q", got, rulesMinorVersion)
	}
}

// TestBuildFromStoreEdgeRulesAdvanceWithAuthoredRule: authoring a second edge
// rule over the store makes BOTH rules ride edge_rules at a higher generation —
// edge_rules tracks the store, not a fixed single constant.
func TestBuildFromStoreEdgeRulesAdvanceWithAuthoredRule(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)
	assetRef := signhash.ContentID(img)
	s := seededStore(t, assetRef)
	ctx := context.Background()

	ds1, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState #1: %v", err)
	}
	snap1, _, err := BuildFromStore(ds1, "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore #1: %v", err)
	}
	if got, want := len(snap1.Sections.EdgeRules.Rules), 1; got != want {
		t.Fatalf("EdgeRules.Rules before authoring = %d, want %d", got, want)
	}

	if _, err := s.Create(ctx, store.KindAutomation, secondEdgeAutomationBody()); err != nil {
		t.Fatalf("author second edge rule: %v", err)
	}

	ds2, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState #2: %v", err)
	}
	snap2, _, err := BuildFromStore(ds2, "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore #2: %v", err)
	}

	if !(snap2.Generation > snap1.Generation) {
		t.Fatalf("generation did not advance after authoring a second edge rule: %d -> %d", snap1.Generation, snap2.Generation)
	}
	if got, want := len(snap2.Sections.EdgeRules.Rules), 2; got != want {
		t.Fatalf("EdgeRules.Rules after authoring a second edge rule = %d, want %d", got, want)
	}
	ids := carriedRuleIDs(t, snap2.Sections.EdgeRules.Rules)
	if !(ids[0] == demoRuleID || ids[1] == demoRuleID) || !(ids[0] == secondEdgeAutomationID || ids[1] == secondEdgeAutomationID) {
		t.Fatalf("carried edge rule ids = %v, want both %q and %q", ids, demoRuleID, secondEdgeAutomationID)
	}

	recomputed, err := hashSections(snap2.Sections)
	if err != nil {
		t.Fatalf("hashSections: %v", err)
	}
	if recomputed != snap2.Hash {
		t.Errorf("recomputed hash %q != snapshot hash %q (REL-053) after authoring an edge rule", recomputed, snap2.Hash)
	}
}

// TestBuildFromStoreEdgeRulesExcludeAppClassifiedRule: an app-classified stored
// rule (a notify action) must NEVER ride edge_rules (REL-062) — only the
// edge-classified seeded demo rule is carried.
func TestBuildFromStoreEdgeRulesExcludeAppClassifiedRule(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)
	assetRef := signhash.ContentID(img)
	s := seededStore(t, assetRef)
	ctx := context.Background()

	if _, err := s.Create(ctx, store.KindAutomation, appClassAutomationBody()); err != nil {
		t.Fatalf("author app-classified rule: %v", err)
	}

	ds, err := s.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	snap, _, err := BuildFromStore(ds, "https://origin.example", id, contentInstant(t), nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}

	if got, want := len(snap.Sections.EdgeRules.Rules), 1; got != want {
		t.Fatalf("EdgeRules.Rules with one app-classified rule authored = %d, want %d (the app rule must NOT ride edge_rules)", got, want)
	}
	ids := carriedRuleIDs(t, snap.Sections.EdgeRules.Rules)
	for _, id := range ids {
		if id == appAutomationID {
			t.Fatalf("app-classified rule %q was carried into edge_rules — REL-062 violation", appAutomationID)
		}
	}

	recomputed, err := hashSections(snap.Sections)
	if err != nil {
		t.Fatalf("hashSections: %v", err)
	}
	if recomputed != snap.Hash {
		t.Errorf("recomputed hash %q != snapshot hash %q (REL-053)", recomputed, snap.Hash)
	}
}

// TestBuildFromStoreCarriesStoredPairingGrants: a pairing grant minted into
// the store (store.AddPairingGrant) rides the very next built snapshot's
// `pairing_grants` section (REL-067) with its REL-121a screen binding intact,
// while a grant already past its ttl at the build instant, and a grant whose
// bound screen row no longer exists, are both dropped from the section.
func TestBuildFromStoreCarriesStoredPairingGrants(t *testing.T) {
	img := loadTestImage(t)
	id := testIdentity(t)
	s := seededStore(t, signhash.ContentID(img))
	ctx := context.Background()
	// The REAL clock, not the fixed daypart instant: AddPairingGrant's own
	// expired-row sweep runs at the store's wall clock, so a fixture issued
	// around a canned past instant would be swept by the very next mint. The
	// screen-program section's resolution is not asserted here, so the
	// instant's daypart position is immaterial.
	now := time.Now().UnixMilli()

	live := wire.PairingGrant{
		GrantID: "grant-11111111111111111111111111111111", Purpose: "pairing",
		ResultingPrincipalKind: "screen", ScreenID: store.SeedScreenID, RelayID: "01J8ZRELAYAAAAAAAAAAAAAAA1",
		TTL: 900, RedemptionMode: "one-time", IssuedAt: now - 1_000,
	}
	expired := wire.PairingGrant{
		GrantID: "grant-22222222222222222222222222222222", Purpose: "pairing",
		ResultingPrincipalKind: "screen", ScreenID: store.SeedScreenID, RelayID: "01J8ZRELAYAAAAAAAAAAAAAAA1",
		TTL: 900, RedemptionMode: "one-time", IssuedAt: now - 901_000,
	}
	for _, g := range []wire.PairingGrant{live, expired} {
		// The scope node is the seeded screen row's own placement — the mint
		// re-checks it in-transaction against the row and refuses any other.
		if err := s.AddPairingGrant(ctx, g, "01J8Z4DEM0SCREENF1RSTPH0TN", "api"); err != nil {
			t.Fatalf("AddPairingGrant(%s): %v", g.GrantID, err)
		}
	}
	// A grant whose screen row is gone by build time: fabricate it directly in
	// the desired-state read rather than deleting a row, so this stays a pure
	// derivation test.
	ds := desiredState(t, s)
	ds.PairingGrants = append(ds.PairingGrants, wire.PairingGrant{
		GrantID: "grant-33333333333333333333333333333333", Purpose: "pairing",
		ResultingPrincipalKind: "screen", ScreenID: "01J8Z9G0NESCREENR0WXXXXXXX", RelayID: "01J8ZRELAYAAAAAAAAAAAAAAA1",
		TTL: 900, RedemptionMode: "one-time", IssuedAt: now - 1_000,
	})

	snap, _, err := BuildFromStore(ds, "https://origin.example", id, now, nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}

	got := snap.Sections.PairingGrants
	if len(got) != 1 {
		t.Fatalf("pairing_grants carries %d grant(s) %v, want exactly the live bound grant", len(got), got)
	}
	if got[0] != live {
		t.Fatalf("pairing_grants[0] = %+v, want %+v (screen binding carried unmodified, REL-121a)", got[0], live)
	}
}
