// Package snapshot builds and signs the feeder's relay/1 desired-state
// generations (`state.snapshot` bodies, REL-051). Wave 1's first photon
// covers the minimal case: one generation carrying exactly one
// screen-program that shows one image, over the feeder's own signing
// identity (internal/feeder/signing).
//
// Canonicalization (no separate spec to consult beyond this package's own
// behavior — a later relay-side verifier, internal/relay/desiredstate,
// reproduces both by calling the exact same shared helpers this package
// does, so the two sides cannot drift apart):
//
//   - `hash` (REL-053) is sha256 over encoding/json's marshaling of the
//     wire.Sections value, computed via wire.HashSections. encoding/json
//     marshals struct fields in their Go declaration order, so
//     byte-identical Sections content always marshals to byte-identical
//     bytes, and therefore the same hash; struct-marshal order IS the
//     canonical form for this wire version.
//   - `signature` (REL-075) is an ed25519 signature over
//     wire.SignedScopeBytes(generation, hash) — encoding/json's marshaling
//     of {generation, hash} in that declaration order — never hash alone —
//     so relabeling a validly signed snapshot under a different generation
//     number changes the signed bytes and invalidates the old signature.
//     The signature is encoded for the wire via wire.EncodeSignature
//     (base64-standard; relay/1 gives no explicit signature-field grammar
//     beyond "a signature" — base64-std is this codec's own choice).
//
// wire.HashSections, wire.SignedScopeBytes, and wire.EncodeSignature all
// live in internal/shared/wire, not here, so this package's signing side
// and internal/relay/desiredstate's verifying side call the exact same
// functions and cannot drift apart on any of the three.
package snapshot

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// SignedSnapshot is the feeder's fully-built, signed relay/1
// `state.snapshot` body (`{generation, hash, signature, sections}`,
// REL-051) — an alias over the shared wire type so callers (and a later
// relay-side verifier) work against the exact contract field names.
type SignedSnapshot = wire.StateSnapshotBody

// Wave 1 first-photon placeholders: this task builds exactly one
// screen-program for a single hard-coded screen, ahead of any real
// screen-registration/pairing task that would supply these IDs and a real
// content-URL TTL policy.
const (
	firstPhotonScreenID        = "screen-first-photon"
	firstPhotonProgramRevision = "rev-1"
	firstPhotonExpiresAt       = 0 // no TTL policy defined yet this wave
)

// Wave-1 first-automation edge_rules placeholders (REL-062). The feeder
// emits one hard-coded demo edge rule ahead of any real
// automation-authoring surface: "when the screen entity turns on, launch
// the dev channel on it" — a state trigger (to:["on"]) driving a
// device_command launch, which the rules/1 compiler classifies edge-class.
// The entity/rule IDs are the same fixture ULIDs the internal/relay/
// automation end-to-end proof uses, so the relay can resolve the trigger's
// entity to a media-player device via the fixture registry.
//
// rulesMinorVersion names the rules/1 minor this rule was authored against
// (contracts/rules-1.md is Version 1.0), carried in the section's
// rules_minor_version per REL-062.
const (
	demoRuleEntityID  = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"
	demoRuleID        = "01J8Z3K4N5P6Q7R8S9T0V1AUTO"
	rulesMinorVersion = "1.0"
)

// firstPhotonSiteEffective is the Wave-1 first-photon site's placement data
// the feeder persists into every snapshot's revocation_and_site.site_effective
// (REL-066), so a relay's dayparting and sun/time evaluation stay correct
// across a restart from the persisted snapshot alone, without first
// completing a fresh hello. It mirrors cmd/waiveo-feeder's firstPhotonSite
// (REL-036) — a real IANA zone and coordinates — standing in for the
// per-site record the data-model site source supplies in a later wave.
var firstPhotonSiteEffective = wire.SiteEffective{
	TZ:   "America/Chicago",
	Lat:  41.8781,
	Long: -87.6298,
}

// demoEdgeRuleJSON is the single authored rules/1 rule the Wave-1 feeder
// signs into every generation's edge_rules section (REL-062). It is carried
// opaquely to the relay, which compiles + loads it (Task 2). Kept as a
// package var (not a const) so it can be marshaled to json.RawMessage once
// at Build time.
var demoEdgeRuleJSON = json.RawMessage(`{"id":"` + demoRuleID + `","mode":"single","triggers":[{"type":"state","entity_id":"` + demoRuleEntityID + `","to":["on"]}],"conditions":[],"actions":[{"type":"device_command","entity_id":"` + demoRuleEntityID + `","command":"launch","params":{"channel":"dev"}}]}`)

// Build builds and signs generation 1 of a relay/1 desired-state
// snapshot carrying exactly one screen-program that shows img: one
// `content` item whose `asset_ref` is img's sha256 content ID
// (signhash.ContentID) and whose `url` resolves to the content origin's
// `/content/<hex>` route under contentBaseURL. It signs with id's
// signing private key.
//
// contentBaseURL also rides `sections.revocation_and_site.content_origin`
// (REL-061/066) verbatim — the same base URL a relay-side schedule resolver
// (internal/relay/schedulehost) later derives schedule-resolved content URLs
// from, so the app-authored and schedule-resolved paths share one origin.
//
// The `sections.edge_rules` section (REL-062) carries the Wave-1
// first-automation demo rule (demoEdgeRuleJSON) under rules_minor_version
// "1.0" — one edge rule the relay compiles + loads into its edge engine
// (internal/relay/automationhost, Task 2). Like every other section it is
// included ahead of hashing/signing, so a tampered edge_rules section is
// caught by the same hash/signature check as any other.
//
// grants populates `sections.pairing_grants` (REL-067) — typically the
// single grant.Mint() record a later Task 6 rides to the relay. A nil
// grants is normalized to a non-nil empty slice, so the section always
// marshals as `[]`, never `null` (REL-060). grants is included in
// `sections` ahead of hashing/signing, so it is covered by `hash`
// (REL-053) and transitively by `signature` (REL-075) exactly like every
// other section.
func Build(img []byte, contentBaseURL string, id *signing.Identity, grants []wire.PairingGrant) (SignedSnapshot, error) {
	if id == nil {
		return SignedSnapshot{}, fmt.Errorf("snapshot: Build: id must not be nil")
	}

	if grants == nil {
		grants = []wire.PairingGrant{}
	}

	assetRef := signhash.ContentID(img)
	hexDigest := strings.TrimPrefix(assetRef, "sha256:")

	scheduleSection, err := buildDemoScheduleSection(assetRef)
	if err != nil {
		return SignedSnapshot{}, fmt.Errorf("snapshot: Build: demo schedule: %w", err)
	}

	sections := wire.Sections{
		ScreenPrograms: []wire.ScreenProgram{
			{
				ScreenID:        firstPhotonScreenID,
				ProgramRevision: firstPhotonProgramRevision,
				Priority:        "scheduled",
				Display:         "content",
				Content: []wire.ContentRef{
					{
						AssetRef:  assetRef,
						URL:       contentBaseURL + "/content/" + hexDigest,
						ExpiresAt: firstPhotonExpiresAt,
					},
				},
			},
		},
		EdgeRules: wire.EdgeRules{
			RulesMinorVersion: rulesMinorVersion,
			Rules:             []json.RawMessage{demoEdgeRuleJSON},
		},
		DeviceInventory: wire.DeviceInventory{
			Devices:           []json.RawMessage{},
			PackMatchPatterns: []json.RawMessage{},
		},
		// The schedule section (REL-065) carries the Wave-2 demo schedule
		// (buildDemoScheduleSection): a two-daypart schedule on the
		// first-photon screen's scope node the relay resolves per-instant
		// (internal/relay/schedulehost, a later task) into a time-varying
		// display:content/display:blank Lease. All seven scheduling-core row
		// arrays are present and non-null (REL-060) — Normalized() guarantees
		// each unused array (validity_windows, fallbacks) marshals as `[]`.
		Schedule: scheduleSection,
		RevocationAndSite: wire.RevocationAndSite{
			Revoked:       []string{},
			SiteEffective: firstPhotonSiteEffective,
			ContentOrigin: contentBaseURL,
		},
		PairingGrants:      grants,
		WorkflowGeneration: nil, // RESERVED, REL-068
	}

	hash, err := hashSections(sections)
	if err != nil {
		return SignedSnapshot{}, err
	}

	const generation = 1

	signature, err := signGenerationHash(generation, hash, id)
	if err != nil {
		return SignedSnapshot{}, err
	}

	return SignedSnapshot{
		Generation: generation,
		Hash:       hash,
		Signature:  signature,
		Sections:   sections,
	}, nil
}

// BuildFromStore builds and signs a relay/1 desired-state snapshot from the app
// store's authored rows (rows, a consistent read at the store's generation, from
// store.DesiredState). It is the store-derived counterpart to Build: the
// `schedule` section (REL-065) carries the store's scope nodes + scheduling-core
// rows (no longer the hardcoded buildDemoScheduleSection), and
// `revocation_and_site.site_effective` (REL-066) is the site node's own
// tz/lat/long (rows.SiteEffective, derived in the store from the SITE scope node
// per data-model DAT-033 — never the feeder's OS locale), and
// `revocation_and_site.content_origin` (REL-061/066) is contentBaseURL verbatim,
// exactly as Build emits it. The `edge_rules` section (REL-062) carries
// rows.EdgeRules — the store's edge-classified authored automations
// (store.EdgeRuleBodies, no longer the hardcoded demoEdgeRuleJSON constant): an
// app-classified stored rule is never carried here (its execution is app-side,
// deferred). Every other section keeps the exact baseline shape Build produces:
// one image screen-program showing img (asset_ref = img's content id, url under
// contentBaseURL), an empty device_inventory, grants in pairing_grants, and the
// reserved workflow_generation.
//
// The snapshot's `generation` is the store's own monotonic counter
// (rows.Generation) rather than a constant, so an api write that advances the
// store generation yields a higher-generation snapshot on the next build — the
// seam that carries an authored edit to the relay. The REL-053 byte-identical-
// marshaling → hash invariant and the REL-075 signature-over-{generation, hash}
// are preserved: this reuses the exact same wire helpers Build does
// (hashSections / signGenerationHash), so signing here and verifying on the relay
// (internal/relay/desiredstate) cannot drift.
func BuildFromStore(rows store.DesiredStateResult, img []byte, contentBaseURL string, id *signing.Identity, grants []wire.PairingGrant) (SignedSnapshot, error) {
	if id == nil {
		return SignedSnapshot{}, fmt.Errorf("snapshot: BuildFromStore: id must not be nil")
	}

	if grants == nil {
		grants = []wire.PairingGrant{}
	}

	assetRef := signhash.ContentID(img)
	hexDigest := strings.TrimPrefix(assetRef, "sha256:")

	scheduleSection, err := scheduleSectionFromStore(rows)
	if err != nil {
		return SignedSnapshot{}, fmt.Errorf("snapshot: BuildFromStore: schedule section: %w", err)
	}

	// edge_rules (REL-062) is the store's own edge-classified automations
	// (rows.EdgeRules, from store.EdgeRuleBodies). Rules is defensively
	// normalized to a non-nil empty slice (REL-060: a store with zero edge
	// rules must still marshal `[]`, never `null`) — DesiredState already
	// guarantees this via EdgeRuleBodies, but BuildFromStore does not trust a
	// caller-assembled DesiredStateResult to have done so.
	edgeRules := rows.EdgeRules
	if edgeRules.Rules == nil {
		edgeRules.Rules = []json.RawMessage{}
	}

	sections := wire.Sections{
		ScreenPrograms: []wire.ScreenProgram{
			{
				ScreenID:        firstPhotonScreenID,
				ProgramRevision: firstPhotonProgramRevision,
				Priority:        "scheduled",
				Display:         "content",
				Content: []wire.ContentRef{
					{
						AssetRef:  assetRef,
						URL:       contentBaseURL + "/content/" + hexDigest,
						ExpiresAt: firstPhotonExpiresAt,
					},
				},
			},
		},
		EdgeRules: edgeRules,
		DeviceInventory: wire.DeviceInventory{
			Devices:           []json.RawMessage{},
			PackMatchPatterns: []json.RawMessage{},
		},
		Schedule: scheduleSection,
		RevocationAndSite: wire.RevocationAndSite{
			Revoked:       []string{},
			SiteEffective: rows.SiteEffective,
			ContentOrigin: contentBaseURL,
		},
		PairingGrants:      grants,
		WorkflowGeneration: nil, // RESERVED, REL-068
	}

	hash, err := hashSections(sections)
	if err != nil {
		return SignedSnapshot{}, err
	}

	generation := rows.Generation

	signature, err := signGenerationHash(generation, hash, id)
	if err != nil {
		return SignedSnapshot{}, err
	}

	return SignedSnapshot{
		Generation: generation,
		Hash:       hash,
		Signature:  signature,
		Sections:   sections,
	}, nil
}

// scheduleSectionFromStore assembles the REL-065 `schedule` section from a store
// desired-state read: the scope nodes marshaled back to raw JSON (the relay
// re-parses them through data-model/1, so a typed round-trip is fine) and the six
// scheduling-core row-kind arrays carried verbatim as the store holds them. The
// result is Normalized so every one of the seven arrays marshals as `[]` rather
// than `null` (REL-060), exactly as buildDemoScheduleSection does.
func scheduleSectionFromStore(rows store.DesiredStateResult) (wire.ScheduleSection, error) {
	scopeNodesRaw := make([]json.RawMessage, 0, len(rows.ScopeNodes))
	for _, n := range rows.ScopeNodes {
		b, err := json.Marshal(n)
		if err != nil {
			return wire.ScheduleSection{}, fmt.Errorf("marshal scope node %s: %w", n.ID, err)
		}
		scopeNodesRaw = append(scopeNodesRaw, b)
	}
	return wire.ScheduleSection{
		ScopeNodes:      scopeNodesRaw,
		Playlists:       rows.Rows.Playlists,
		Schedules:       rows.Rows.Schedules,
		ValidityWindows: rows.Rows.ValidityWindows,
		Dayparts:        rows.Rows.Dayparts,
		Fallbacks:       rows.Rows.Fallbacks,
		PresetBatches:   rows.Rows.PresetBatches,
	}.Normalized(), nil
}

// hashSections computes REL-053's `hash` by delegating to
// wire.HashSections — THE single shared canonicalization a later
// relay-side verifier (internal/relay/desiredstate) also calls, so signing
// and verifying cannot drift apart on it.
func hashSections(sections wire.Sections) (string, error) {
	return wire.HashSections(sections)
}

// generationHashCanonBytes marshals the REL-075 signed scope for a given
// generation and hash by delegating to wire.SignedScopeBytes — the exact
// bytes Build signs (and a verifier must reproduce to check a signature),
// from the same shared helper a later relay-side verifier calls.
func generationHashCanonBytes(generation int64, hash string) ([]byte, error) {
	return wire.SignedScopeBytes(generation, hash)
}

// signGenerationHash computes REL-075's `signature`: an ed25519 signature
// over generationHashCanonBytes(generation, hash), encoded for the wire via
// wire.EncodeSignature — the shared codec both this (signing) side and a
// later relay-side verifier must use, so they cannot drift.
func signGenerationHash(generation int64, hash string, id *signing.Identity) (string, error) {
	canon, err := generationHashCanonBytes(generation, hash)
	if err != nil {
		return "", err
	}
	sig := signhash.Sign(id.SigningPriv(), canon)
	return wire.EncodeSignature(sig), nil
}
