// Package snapshot builds and signs the feeder's relay/1 desired-state
// generations (`state.snapshot` bodies, REL-051), over the feeder's own
// signing identity (internal/feeder/signing).
//
// There are two builders, and only one of them ships in the running feeder:
//
//   - BuildFromStore derives EVERY section from the app store's authored rows —
//     including one `screen_programs` entry per screen identity row, resolved
//     through the data-model/1 scheduling core at that screen's own placement
//     (screenprograms.go). This is the builder the feeder uses, and the reason
//     an operator's edit reaches a screen.
//   - Build/BuildCast are the store-less FIXTURE builders, for the paths that
//     have no store to derive from (the conformance drivers, the virtual-player
//     first-light proof). They assign one program, showing the cast they are
//     handed, to one fixture screen.
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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// SignedSnapshot is the feeder's fully-built, signed relay/1
// `state.snapshot` body (`{generation, hash, signature, sections}`,
// REL-051) — an alias over the shared wire type so callers (and a later
// relay-side verifier) work against the exact contract field names.
type SignedSnapshot = wire.StateSnapshotBody

// fixtureScreenID is the screen the STORE-LESS builders (Build/BuildCast)
// assign their one program to: the id of the real screen identity row
// store.SeedDemo inserts (DAT-004/DAT-004a), single-sourced from the store
// package so the fixture path and a freshly-seeded store name the same screen
// rather than two.
//
// Those builders exist for the paths that have no store to derive from — the
// conformance drivers and the virtual-player first-light proof — and a fixture
// screen is still a screen: it needs an id that is a canonical ULID (DAT-005a)
// and that resolves to a screen ROW, not a `screen`-kind scope node (DAT-004a).
// The store-derived path does not use this at all; it names every screen from
// the row it resolved (DeriveScreenPrograms).
//
// It is exported as FixtureScreenID for the harnesses that must know which
// screen a fixture snapshot's one program is for — a relay serves a program
// PER SCREEN, so a fixture player that pairs into some other screen id is
// served the terminal default (data-model/1 DAT-118) rather than the fixture
// program, which reads as a mysteriously blank screen rather than as the
// mismatch it is.
const fixtureScreenID = store.SeedScreenID

// FixtureScreenID is fixtureScreenID, exported for out-of-package harnesses.
const FixtureScreenID = fixtureScreenID

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
	demoRuleID        = "01J8Z3K4N5P6Q7R8S9T0V1AT01"
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

// CastItem is one ordered item of a feeder-built screen-program's `content`
// array (REL-061/061a) — the feeder-side input BuildCast
// turns into a wire.ContentRef. Bytes MUST already be (or be about to be)
// added to the content origin (internal/feeder/origin.Store.Add) under the
// SAME bytes, so the asset_ref this package computes (signhash.ContentID(
// Bytes)) names content the origin can actually Serve — BuildCast itself
// never touches the origin; a caller adds the bytes first (or verifies they
// already are present) and passes them here to compute the matching
// reference.
//
// ContentType is carried verbatim into the built ContentRef's `content_type`
// (REL-061a) — this package performs no validation against player/1's
// content-type floor (PLY-014); a caller is expected to pass one of `image`,
// `video`, or another value player/1 treats as forward-compatible data
// (PLY-016). DurationMS is carried verbatim into `duration_ms` (REL-061a); 0
// means unspecified, matching that field's own optional-override semantics.
type CastItem struct {
	Bytes       []byte
	ContentType string
	DurationMS  int64
}

// contentRefFor builds the wire.ContentRef a single CastItem resolves to,
// under contentBaseURL — the same `<base>/content/<hex>` URL grammar every
// content-serving path in this codebase uses (REL-061/140).
func contentRefFor(item CastItem, contentBaseURL string) wire.ContentRef {
	assetRef := signhash.ContentID(item.Bytes)
	hexDigest := strings.TrimPrefix(assetRef, "sha256:")
	return wire.ContentRef{
		AssetRef:    assetRef,
		URL:         contentBaseURL + "/content/" + hexDigest,
		ExpiresAt:   contentURLExpiresAt,
		ContentType: item.ContentType,
		DurationMS:  item.DurationMS,
	}
}

// Build builds and signs generation 1 of a relay/1 desired-state
// snapshot carrying exactly one screen-program that shows img: one
// `content` item whose `asset_ref` is img's sha256 content ID
// (signhash.ContentID) and whose `url` resolves to the content origin's
// `/content/<hex>` route under contentBaseURL. It signs with id's
// signing private key.
//
// Build is BuildCast's single-item convenience wrapper: it carries no
// `content_type`/`duration_ms` (CastItem{Bytes: img} — both left at their
// zero value), which ContentRef's own `omitempty` tags marshal as absent
// keys (wire.ContentRef) — so every existing caller of Build keeps producing
// the exact same wire bytes, and the exact same `hash` (REL-053), as before
// those fields existed.
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
	snap, err := BuildCast([]CastItem{{Bytes: img}}, contentBaseURL, id, grants)
	if err != nil {
		return SignedSnapshot{}, fmt.Errorf("snapshot: Build: %w", err)
	}
	return snap, nil
}

// BuildCast is Build's ordered-multi-item generalization: it builds and
// signs generation 1 of a relay/1 desired-state snapshot carrying exactly
// one screen-program whose `content` array (REL-061) holds one ContentRef
// per element of items, IN ORDER — a real multi-item cast (an ordered list
// of images/videos, each independently asset_ref-verifiable) rather than
// Build's single hard-coded image. Every item's own ContentType/DurationMS
// rides into its ContentRef's `content_type`/`duration_ms` (REL-061a)
// unmodified (contentRefFor). items MUST be non-empty; BuildCast does not
// itself add any item's bytes to a content origin (internal/feeder/origin) —
// a caller does that first, so this function's computed asset_ref/url
// actually resolve to servable bytes.
//
// The demo schedule section (REL-065, buildDemoScheduleSection) still
// carries a single-item playlist referencing items[0]'s own asset_ref — the
// schedule/playlist resolution path (internal/relay/schedulehost) is a
// separate, already multi-item-capable mechanism (data-model/1 DAT-041) this
// function does not touch; only the direct screen_programs.content array
// gets the full ordered cast.
//
// Build(img, ...) is exactly BuildCast([]CastItem{{Bytes: img}}, ...): a
// single item with no ContentType/DurationMS set, which ContentRef's own
// `omitempty` tags marshal as absent keys — so a 1-item BuildCast call
// produces byte-identical wire output, and therefore an identical `hash`
// (REL-053), to Build's own pre-existing output.
func BuildCast(items []CastItem, contentBaseURL string, id *signing.Identity, grants []wire.PairingGrant) (SignedSnapshot, error) {
	if id == nil {
		return SignedSnapshot{}, fmt.Errorf("snapshot: BuildCast: id must not be nil")
	}
	if len(items) == 0 {
		return SignedSnapshot{}, fmt.Errorf("snapshot: BuildCast: items must be non-empty")
	}

	// A fixture snapshot names its own screen, so its pairing grants name it
	// too: an unbound grant redeems into a freshly-invented opaque screen id
	// (REL-121's baseline shape), and a relay serves a program PER SCREEN, so a
	// player pairing on one would be served data-model/1's terminal default
	// instead of the program this very snapshot carries. That screen binding is
	// REL-121a's requirement, and it is the only one this fills in.
	//
	// It is NOT REL-121c. That clause is about `relay_id` — an app peer MUST
	// bind every one-time grant to exactly one relay, and MUST NOT deliver an
	// unbound one — and these grants are still delivered unbound, so they remain
	// exactly what REL-121c forbids. Nothing here can honestly fix that: this is
	// the demo/fixture builder, which has no connected-relay set to choose from
	// and no caller-named relay to carry, and REL-121c's own second consequence
	// is that a binding chosen on the caller's behalf surfaces at redemption as
	// an ordinary invalid-code refusal. The real issuing surface
	// (internal/app/store's pairing grants, which takes the relay from the
	// caller) is where REL-121c is satisfied; a fixture that stamped some relay
	// id here would look conformant and mint codes nobody can redeem.
	//
	// A grant that already names a screen is left alone — only an unstated
	// binding is filled in — and the caller's own slice is never mutated.
	bound := make([]wire.PairingGrant, 0, len(grants))
	for _, g := range grants {
		if g.ScreenID == "" {
			g.ScreenID = fixtureScreenID
		}
		bound = append(bound, g)
	}
	grants = bound

	content := make([]wire.ContentRef, 0, len(items))
	for _, item := range items {
		content = append(content, contentRefFor(item, contentBaseURL))
	}

	// The demo schedule's own playlist still shows a single item — the
	// first of the cast — exactly as Build's pre-existing behavior did when
	// len(items) == 1 (BuildCast's doc above).
	scheduleSection, err := buildDemoScheduleSection(content[0].AssetRef)
	if err != nil {
		return SignedSnapshot{}, fmt.Errorf("demo schedule: %w", err)
	}

	// The fixture program: the whole cast, shown on the fixture screen. Its
	// program_revision is derived from the program itself (programRevisionFor),
	// the same derivation the store-driven path uses — so a fixture built from
	// different bytes carries a different revision, and one rebuilt from the
	// same bytes reproduces it exactly.
	fixtureProgram := wire.ScreenProgram{
		ScreenID: fixtureScreenID,
		Priority: leasePriorityScheduled,
		Display:  displayContent,
		Content:  content,
	}
	fixtureProgram.ProgramRevision = programRevisionFor(fixtureProgram)

	sections := wire.Sections{
		ScreenPrograms: []wire.ScreenProgram{fixtureProgram},
		EdgeRules: wire.EdgeRules{
			RulesMinorVersion: rulesMinorVersion,
			Rules:             []json.RawMessage{demoEdgeRuleJSON},
		},
		// device_inventory (REL-063/064) is empty on this store-less path:
		// both arrays are derived from authored state (the adopted-device
		// rows and the installed packs), and this builder has no store to
		// derive them from. Normalized() keeps both arrays `[]` rather than
		// `null` (REL-060). The store-derived path (BuildFromStore)
		// carries the real inventory.
		DeviceInventory: wire.DeviceInventory{}.Normalized(),
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
// store.DesiredState) — the store-derived counterpart to Build, and the only
// builder the running feeder uses. EVERY section it emits is derived from
// authored state:
//
//   - `screen_programs` (REL-061) is ONE entry per screen identity row, each
//     resolved through the data-model/1 scheduling core at that screen's own
//     placement and at nowMs (DeriveScreenPrograms, screenprograms.go). Editing a
//     playlist, a daypart, a schedule, or a screen's placement therefore changes
//     what that screen is delivered on the next rebuild — which is the whole
//     point of the section, and what a single hardcoded entry could never do.
//   - `schedule` (REL-065) carries the store's scope nodes + scheduling-core rows,
//     so the relay can re-resolve the same rows per-instant between generations.
//   - `edge_rules` (REL-062) carries rows.EdgeRules — the store's edge-classified
//     authored automations; an app-classified stored rule is never carried here.
//   - `device_inventory` (REL-063/064) carries rows.DeviceInventory: the store's
//     adopted-device rows projected into REL-063 entries — each carrying its
//     per-entity enabled/hidden/display_name/category policy, decisions no
//     discovery sweep can report — alongside the discovery-match patterns every
//     INSTALLED PACK declares (REL-064), which are watched for independent of
//     what is already adopted.
//   - `revocation_and_site` (REL-066) carries the site node's own tz/lat/long
//     (rows.SiteEffective, per DAT-033 — never the feeder's OS locale) and
//     contentBaseURL as the content origin.
//   - `pairing_grants` (REL-067) carries the store's pending pairing-grant
//     records (rows.PairingGrants, store.AddPairingGrant), filtered to those
//     still inside their ttl at nowMs and — for a screen-bound grant
//     (REL-121a) — to those whose screen row still exists in rows.Screens: a
//     grant for a since-deleted screen, or one already past its ttl, stops
//     riding new generations here, while the relay's own ttl enforcement
//     (REL-121/122) remains the authority for grants it already holds.
//
// nowMs is the instant the generation's programs are resolved at, injected rather
// than read from a clock here: scheduling is per-instant by construction
// (DAT-111), so the instant is an INPUT to the snapshot, and a caller that wants
// a reproducible generation passes a fixed one. Because it is an input, a rebuild
// of the SAME store generation at a materially later instant — one that crosses a
// daypart boundary — can differ from the first build of it. That is not a
// versioning hazard in practice: the feeder caches its built snapshot by store
// generation, and a relay ignores a pull that does not strictly advance the
// generation it has (REL-052/070), so a given generation is applied exactly once.
// What keeps a screen correct BETWEEN generations is not this section at all —
// it is the relay's own resolver re-reading the `schedule` section carried
// alongside it (internal/relay/schedulehost).
//
// The returned []datamodel.Error is DeriveScreenPrograms' per-screen degrade
// list, not a build failure: a screen whose effective tz cannot be resolved is
// omitted from the section (DAT-034 — never resolved against a substituted zone)
// and reported here for the caller to log. The snapshot itself is complete and
// signed regardless.
//
// The snapshot's `generation` is the store's own monotonic counter
// (rows.Generation) rather than a constant, so an api write that advances the
// store generation yields a higher-generation snapshot on the next build — the
// seam that carries an authored edit to the relay. The REL-053 byte-identical-
// marshaling → hash invariant and the REL-075 signature-over-{generation, hash}
// are preserved: this reuses the exact same wire helpers Build does
// (hashSections / signGenerationHash), so signing here and verifying on the relay
// (internal/relay/desiredstate) cannot drift.
func BuildFromStore(rows store.DesiredStateResult, contentBaseURL string, id *signing.Identity, nowMs int64, contentURLKey []byte) (SignedSnapshot, []datamodel.Error, error) {
	if id == nil {
		return SignedSnapshot{}, nil, fmt.Errorf("snapshot: BuildFromStore: id must not be nil")
	}

	grants := deliverablePairingGrants(rows, nowMs)

	screenPrograms, degrades := DeriveScreenPrograms(rows, contentBaseURL, nowMs)

	scheduleSection, err := scheduleSectionFromStore(rows)
	if err != nil {
		return SignedSnapshot{}, degrades, fmt.Errorf("schedule section: %w", err)
	}

	// edge_rules (REL-062) is the store's own edge-classified automations
	// (rows.EdgeRules, from store.EdgeRuleBodies). Rules is defensively
	// normalized to a non-nil empty slice (REL-060: a store with zero edge
	// rules must still marshal `[]`, never `null`) — DesiredState already
	// guarantees this via EdgeRuleBodies, but BuildFromStore does not
	// trust a caller-assembled DesiredStateResult to have done so.
	edgeRules := rows.EdgeRules
	if edgeRules.Rules == nil {
		edgeRules.Rules = []json.RawMessage{}
	}

	sections := wire.Sections{
		ScreenPrograms: screenPrograms,
		EdgeRules:      edgeRules,
		// device_inventory (REL-063/064) is the store's own derivation
		// (rows.DeviceInventory, store.deviceInventory): the adopted-device
		// rows projected into REL-063 entries — carrying each entity's
		// authored enabled/hidden/display_name/category policy — alongside
		// the discovery-match patterns every INSTALLED PACK declares
		// (REL-064), which REL-064 requires be watched for "independent of
		// what is already adopted". Normalized defensively so a
		// caller-assembled DesiredStateResult that left an array nil still
		// marshals `[]`, never `null` (REL-060) — DesiredState already
		// guarantees it.
		DeviceInventory: rows.DeviceInventory.Normalized(),
		Schedule:        scheduleSection,
		RevocationAndSite: wire.RevocationAndSite{
			// REL-066: the revoked ids the store holds (api/1 API-140's authoring half),
			// carried in the SIGNED snapshot so a relay enforces them while
			// disconnected from its app peer — which is the case the enforcement
			// was built for.
			Revoked:       revokedOrEmpty(rows.Revoked),
			SiteEffective: rows.SiteEffective,
			ContentOrigin: contentBaseURL,
			// REL-066a: the key every relay at this site MINTS content URLs
			// with. base64 so it rides JSON; omitted entirely when unset, which
			// keeps the REL-053 hash byte-identical for a deployment that has
			// not turned signing on.
			ContentURLKey: encodeContentURLKey(contentURLKey),
		},
		PairingGrants:      grants,
		WorkflowGeneration: nil, // RESERVED, REL-068
	}

	hash, err := hashSections(sections)
	if err != nil {
		return SignedSnapshot{}, degrades, err
	}

	generation := rows.Generation

	signature, err := signGenerationHash(generation, hash, id)
	if err != nil {
		return SignedSnapshot{}, degrades, err
	}

	return SignedSnapshot{
		Generation: generation,
		Hash:       hash,
		Signature:  signature,
		Sections:   sections,
	}, degrades, nil
}

// deliverablePairingGrants filters the store's pending pairing-grant records
// down to the ones a snapshot built at nowMs should still deliver (REL-067):
// a grant past its own ttl at nowMs is dropped (there is nothing left for a
// relay to redeem, and REL-121/122 make the relay enforce ttl on whatever it
// already holds), and a screen-bound grant (REL-121a) whose screen row no
// longer exists in rows.Screens is dropped too — its redemption would mint a
// credential for a row nothing resolves anymore. The result is always
// non-nil, so the section marshals `[]`, never `null` (REL-060), and — since
// rows.PairingGrants arrives in stored (issued_at, grant_id) order and this
// filter preserves order — a deterministic function of (rows, nowMs), which
// the snapshot hash (REL-053) requires.
func deliverablePairingGrants(rows store.DesiredStateResult, nowMs int64) []wire.PairingGrant {
	screens := make(map[string]bool, len(rows.Screens))
	for _, sc := range rows.Screens {
		screens[sc.ID] = true
	}
	out := []wire.PairingGrant{}
	for _, g := range rows.PairingGrants {
		if g.IssuedAt+g.TTL*1000 <= nowMs {
			continue
		}
		if g.ScreenID != "" && !screens[g.ScreenID] {
			continue
		}
		out = append(out, g)
	}
	return out
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

// encodeContentURLKey renders the content-URL signing key for the wire
// (REL-066a), yielding "" for an absent key so the field is omitted rather than
// carried as an empty string a relay would have to special-case.
func encodeContentURLKey(key []byte) string {
	if len(key) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(key)
}

// revokedOrEmpty normalizes a nil revoked list to an empty one.
//
// REL-060 requires the section to marshal as `[]`, never `null`, and a caller
// that builds a DesiredStateResult by hand — every test that does — would
// otherwise change the snapshot's bytes and therefore its REL-053 hash purely
// by leaving a field at its zero value.
func revokedOrEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}
