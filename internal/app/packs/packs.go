// Package packs is the app-side install pipeline for declarative packs: it turns
// an untrusted pack artifact (a zip carrying a manifest/1 manifest, the pack's
// ui-schema/1 page documents, and its locale catalogs) into store-backed rows —
// gated, end to end, by the REAL manifest engine (internal/manifest.Validate)
// running against genuinely-populated host registries.
//
// A pack is DATA, not code. Nothing in an artifact is ever executed, required,
// or evaluated — the manifest is validated, the page documents are parsed as
// JSON and path-matched against the manifest's ui.pages, the locale catalogs are
// parsed as JSON, and all of it is written to the store. That declarative-only
// posture is the whole point of this wave (a sealed pre-ctx/1 platform: nothing
// runs, so there is nothing to sandbox).
//
// The pipeline is the gate; the store is the persistence. Install refuses an
// unsafe/malformed artifact with a typed ArtifactError (→ 422, single message)
// and a manifest-invalid one with a ManifestError carrying the contract's typed
// per-field errors (→ 422, errors[]). Only a clean pack reaches the store, and
// InstallPack writes it atomically (one transaction, one generation bump) — so
// an install is all-or-nothing, never partial.
//
// Provenance is part of the gate (marketplace/1 MKT-009a/MKT-009b): every
// artifact MUST carry a valid signature envelope (internal/packsig) whose
// content digest covers the exact entry set this pipeline extracted — every
// entry but the envelope itself, which is dropped from the bundle as soon as
// it has been verified, so nothing downstream ever reads an entry the digest
// did not cover — and whose
// signing key a trust anchor authorizes for the pack's publisher namespace —
// verified BEFORE the manifest engine runs and before anything persists. An
// unsigned, tampered, or wrong-key artifact refuses with a typed ArtifactError
// (PACK_UNSIGNED / PACK_SIGNATURE_INVALID / PACK_SIGNER_UNTRUSTED) and leaves
// no residue: no row, no file, no generation bump. There is no legacy or
// grandfather path — a pack installed before verification existed re-verifies
// like any other artifact on its next install.
package packs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/manifest"
	"github.com/maaxton/waiveo-next/internal/packsig"
)

// devMemoryFloorMiB is the host-configured minimum resources.memory (MAN-042) a
// pack must request in this dev POC. A pack asking for less than the host is
// willing to guarantee is refused at install with the contract's typed
// RESOURCE_BELOW_FLOOR error.
const devMemoryFloorMiB = 32

// devCapabilities is the minimal host capability registry (MAN-021) this dev POC
// recognizes: a requested capability outside this set is refused with a typed
// UNKNOWN_CAPABILITY error naming it — never a silent grant. It is host
// configuration, not part of manifest/1; the real, evolving registry arrives
// with the security-model + ctx/1 waves.
var devCapabilities = map[string]bool{
	"device.read":        true,
	"device.command":     true,
	"egress.http":        true,
	"notifications.send": true,
	"storage.read":       true,
	"storage.write":      true,
}

// pageDocExt / pageDocDir realize ui-schema/1 UIS-001: a pack's page document
// for a ui.pages[] entry with path P is bundled at "ui/P.json" (each
// "/"-separated segment mapping to a nested path component). This is the single
// place the manifest's declared page path is resolved to its bundled document.
const (
	pageDocDir = "ui/"
	pageDocExt = ".json"
)

// pageDocPath returns the bundle entry a ui.pages[].path resolves to (UIS-001).
func pageDocPath(path string) string { return pageDocDir + path + pageDocExt }

// localePrefix / localeExt bracket a locale catalog's bundle entry: the pack's
// default-locale catalog is "messages/en.json" (MAN-111), and any additional
// locale L is "messages/L.json".
const (
	localePrefix = "messages/"
	localeExt    = ".json"
)

// ManifestError is an install refused by the manifest engine: it carries the
// contract's own typed per-field violations (each a manifest.Error with a
// taxonomy Code, a dotted Field, and a Message). The api layer renders it as a
// 422 Problem whose errors[] extension is exactly these fields — the refusal a
// client can act on field by field (a bad id, an unknown capability, a
// below-floor memory request, a dataModel.version regression, …).
type ManifestError struct {
	Errors []manifest.Error
}

func (e *ManifestError) Error() string {
	return fmt.Sprintf("pack manifest invalid (%d error(s))", len(e.Errors))
}

// Result is a successful install's summary — the pack identity plus what landed
// (the declared collections and the installed page/locale sets) — the shape the
// api 201/200 body and the make-dev smoke line are built from.
type Result struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	Pages       []string `json:"pages"`
	Collections []string `json:"collections"`
	Locales     []string `json:"locales"`
	// Created is true for a fresh install (→ 201) and false for a reinstall that
	// updated an existing pack in place (→ 200). Not serialized (the HTTP status
	// carries it).
	Created bool `json:"-"`
}

// Installer runs the manifest-gated install pipeline over a store. It holds the
// store, the artifact-safety Limits, and the trust-anchor source artifact
// signatures verify against (marketplace/1 MKT-009b); the host registries are
// rebuilt per install (their BundleFiles and InstalledDataModelVersion are
// per-artifact).
type Installer struct {
	store   *store.Store
	limits  Limits
	anchors packsig.TrustAnchors
	// market is the configured registry (marketplace.go), wired by
	// WithMarketplace. Nil — the default — means this deployment installs only
	// from a directly-supplied artifact: a marketplace reference refuses rather
	// than silently resolving against nothing.
	market *Market
}

// InstallerOption configures an Installer at construction.
type InstallerOption func(*Installer)

// WithMarketplace wires the registry a marketplace reference resolves through
// (marketplace/1 MKT-060/060a). It adds a way to LOCATE an artifact; it adds no
// trust — every resolved artifact still passes the same signature envelope
// verification against the same anchors as a hand-uploaded one.
func WithMarketplace(m *Market) InstallerOption {
	return func(in *Installer) { in.market = m }
}

// NewInstaller builds an Installer over st with the default artifact limits,
// verifying every artifact's signature envelope against anchors. anchors is the
// MKT-009b trust-anchor seam: a host-provisioned set today, the root-signed
// publisher-namespace delegation once the external trust root exists — the
// pipeline never knows the difference. A NIL anchors source fails closed: every
// signed artifact refuses PACK_SIGNER_UNTRUSTED (and an unsigned one
// PACK_UNSIGNED), so an unwired deployment can never install a pack, signed or
// not — refusal, never default-permit.
func NewInstaller(st *store.Store, anchors packsig.TrustAnchors, opts ...InstallerOption) *Installer {
	in := &Installer{store: st, limits: DefaultLimits, anchors: anchors}
	for _, opt := range opts {
		opt(in)
	}
	return in
}

// hostRegistries builds the install-time HostRegistries the manifest engine
// validates against: the renderer's four page types (ui-schema/1 UIS-002/003 —
// the registry MAN-010/060 validate against), the built-in device classes
// (device-class-registry/1, MAN-070), the minimal dev capability registry
// (MAN-021), the pack's own bundle file set (so MAN-063 surface entries and the
// MAN-111 messages/en.json check are REAL), the dev memory floor (MAN-042), and
// the currently-installed dataModel.version (MAN-053). Features/providers are
// empty in this POC (a declared feature flag or connection is refused until the
// host registers them) — documented, not a silent grant.
func hostRegistries(bundleFiles map[string]bool, installedDataModelVersion int) manifest.HostRegistries {
	pageTypes := map[string]bool{}
	for _, t := range PageTypes {
		pageTypes[t] = true
	}
	deviceClasses := map[string]bool{}
	for _, name := range deviceclass.Builtin().ClassNames() {
		deviceClasses[name] = true
	}
	return manifest.HostRegistries{
		Capabilities:              devCapabilities,
		Features:                  map[string]bool{},
		PageTypes:                 pageTypes,
		DeviceClasses:             deviceClasses,
		Providers:                 map[string]bool{},
		BundleFiles:               bundleFiles,
		MemoryFloorMiB:            devMemoryFloorMiB,
		InstalledDataModelVersion: installedDataModelVersion,
	}
}

// PageTypes is the renderer's closed page-type set (ui-schema/1 UIS-003) the
// host populates its MAN-010/060 page-type registry with — the four page types
// the Horizon kit renders. Kept here as the Go-side mirror of the web renderer's
// PAGE_TYPES so the install-time registry matches exactly what the console can
// render.
var PageTypes = []string{"list-detail", "settings-form", "dashboard", "wizard"}

// Install validates a raw pack artifact and, only if it is clean, installs it —
// atomically, gated by signature verification and then the real manifest
// engine. It returns a Result on success, an *ArtifactError for an
// unsafe/malformed/unsigned/tampered artifact, or a *ManifestError for a
// manifest the engine refused. It executes NOTHING from the artifact.
func (in *Installer) Install(ctx context.Context, artifact []byte) (Result, error) {
	return in.install(ctx, artifact, nil)
}

// provenance is everything a marketplace resolution contributes to an install
// beyond the artifact's bytes: the identity the resolved index entry CLAIMED
// (checked against the artifact's own signed identity, MKT-040a) and the record
// members only a registry-mediated install can carry (MKT-094/MKT-094a). A
// direct artifact upload passes nil — it still records, with no trust channel
// and no entry digest, per MKT-094a's direct-path clause.
type provenance struct {
	// Source is the registry source id the entry was served from.
	Source string
	// TrustChannel is the reference's pinned trust channel (MKT-060a(b)).
	TrustChannel string
	// The entry's own claimed identity and digest, as published.
	EntryID      string
	EntryKind    string
	EntrySubtype string
	EntryVersion string
	EntryDigest  string
	// StaleSource marks a file://-resolved install (MKT-063/MKT-094).
	StaleSource bool
	// ViaPointer is true when the reference named no version and the entry was
	// selected by following the channel pointer — the only resolution MKT-050's
	// anti-rollback governs. An explicit version pin (MKT-044's archived
	// reinstall) is the operator's own stated choice and is not governed.
	ViaPointer bool
}

// install is the one install path. Every caller — the direct artifact upload and
// the marketplace resolver alike — runs THIS function, so there is no route by
// which a resolved artifact skips the signature gate, the manifest gate, or the
// install record. A resolution supplies extra expectations (prov); it never
// supplies extra trust.
func (in *Installer) install(ctx context.Context, artifact []byte, prov *provenance) (Result, error) {
	return in.installWith(ctx, artifact, prov, "")
}

// installExpecting is install under a compare-and-swap on the version being
// replaced: the pack row MUST still carry expectPriorVersion when the install
// transaction commits. The revert path (update.go) is its only caller — its
// premise ("the applied version has been yanked") is established from a read
// taken outside the write lock, and this is what re-asserts that premise inside
// the transaction instead of trusting it to still hold.
func (in *Installer) installExpecting(ctx context.Context, artifact []byte, prov *provenance, expectPriorVersion string) (Result, error) {
	return in.installWith(ctx, artifact, prov, expectPriorVersion)
}

func (in *Installer) installWith(ctx context.Context, artifact []byte, prov *provenance, expectPriorVersion string) (Result, error) {
	bundle, err := ReadBundle(artifact, in.limits)
	if err != nil {
		return Result{}, err
	}

	// MKT-009a/b: verify the artifact's signature envelope over the EXACT entry
	// set ReadBundle extracted — the map this pipeline consumes from here on —
	// before the manifest engine ever runs and before anything can persist. The
	// digest-over-extracted-set is what makes "verified == installed" true: a
	// zip whose container encoding would present an entry differently to some
	// other parser cannot matter, because what is digested is what installs.
	env, err := verifyArtifactSignature(bundle, in.anchors)
	if err != nil {
		return Result{}, err
	}
	// The envelope has done its job. Drop it so nothing downstream — the
	// MAN-063 file-set registry, page/locale collection, or anything added
	// later — can ever read the one entry the digest does not cover.
	bundle.drop(packsig.EnvelopeName)

	manifestBytes, ok := bundle.File("manifest.json")
	if !ok {
		return Result{}, artifactErr("PACK_MANIFEST_MISSING",
			"the artifact does not contain a manifest.json at its root")
	}
	var m manifest.PackManifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return Result{}, artifactErr("PACK_MANIFEST_INVALID",
			"manifest.json is not valid JSON: %v", err)
	}

	// The bundled manifest MUST be the identity that was signed (MKT-009a): the
	// content digest already covers manifest.json byte-for-byte, so this only
	// fires when the SIGNER produced an envelope naming a different id/version
	// than its own manifest — a mislabeled artifact is refused, never installed
	// under either identity. This is also the version-binding seam the coming
	// update/rollback machinery leans on: the store row's (id, version) is
	// exactly the signed (artifact_id, version), so a signature for v1 can never
	// vouch for a row claiming v2.
	if m.ID != env.ArtifactID || m.Version != env.Version {
		return Result{}, artifactErr("PACK_SIGNATURE_INVALID",
			"the bundled manifest's identity (%s@%s) differs from the signed identity (%s@%s)",
			m.ID, m.Version, env.ArtifactID, env.Version)
	}

	// MKT-040a: where a resolution named this artifact, the index entry's
	// CLAIMED identity must equal the artifact's SIGNED identity. The entry's
	// digest already proved these are the bytes the entry named, and the
	// envelope already proved those bytes carry this identity — but neither
	// forbids a source from listing a validly-signed artifact under a different
	// entry's identity, which would install, record, pin and auto-track a pack
	// nobody asked for. The signed identity is authoritative here; the entry is
	// the claim being checked.
	if prov != nil {
		if err := checkResolvedIdentity(*prov, env); err != nil {
			return Result{}, err
		}
	}

	// The currently-installed dataModel.version, if this pack is already
	// installed — the input MAN-053's version-regression check needs. Read
	// outside the install transaction (dev-POC: the store is single-writer, so
	// the window is negligible; the manifest engine is still the authority on the
	// refusal).
	var installedVersion int
	if prior, found, gerr := in.store.GetPack(ctx, m.ID); gerr != nil {
		return Result{}, gerr
	} else if found {
		installedVersion = prior.DataModelVersion
	}

	host := hostRegistries(bundle.FileSet(), installedVersion)
	if errs := manifest.Validate(m, host); len(errs) > 0 {
		return Result{}, &ManifestError{Errors: errs}
	}

	// auth-seam: consent (MAN-022) is auto-granted in this dev POC. In the real
	// platform the owner is prompted here to grant the manifest's requested
	// capabilities/egress/resources/connections before the install commits, and
	// the grant is recorded (security-model). The pipeline proceeds as if
	// consent were granted; this is the single seam that change lands at.

	files, pages, locales, err := in.collectBundleFiles(&m, bundle)
	if err != nil {
		return Result{}, err
	}

	spec := store.PackInstall{
		ID:                 m.ID,
		Version:            m.Version,
		DataModelVersion:   m.DataModel.Version,
		Manifest:           append(json.RawMessage(nil), manifestBytes...),
		Files:              files,
		Record:             installRecord(m.Version, env, prov),
		ExpectPriorVersion: expectPriorVersion,
	}
	// A registry-mediated install raises the (pack, trust channel) high-water
	// mark MKT-050's anti-rollback reads — in the install transaction, so a
	// refusal further down advances nothing. A direct upload pins no channel and
	// therefore marks nothing (MKT-094a).
	if prov != nil {
		spec.ChannelMark = &store.ChannelMarkAdvance{
			TrustChannel: prov.TrustChannel,
			Version:      m.Version,
			Higher:       VersionHigher,
			ViaPointer:   prov.ViaPointer,
		}
	}
	// Re-assert MAN-053 inside the install transaction, against the prior row read
	// under the store's write lock — not the installedVersion snapshot read above.
	// Two concurrent installs of a not-yet-installed id both read installedVersion=0
	// (so manifest.Validate skips the regression check for both); whichever commits
	// LAST would otherwise overwrite the row unconditionally, silently downgrading
	// the dataModel.version. This guard catches that in the committing transaction
	// and refuses it with the SAME typed error the sequential path surfaces.
	guards := []store.InstallGuard{versionRegressionGuard(m.DataModel.Version)}
	if prov != nil && prov.ViaPointer {
		guards = append(guards, pointerDowngradeGuard(m.Version))
	}
	pack, created, err := in.store.InstallPack(ctx, spec, guards...)
	if err != nil {
		// The concurrent-install half of MKT-050: another install raised this
		// pair's mark between the resolver's read and this transaction, so the
		// mark refused inside the tx. Same refusal the sequential path gives.
		if errors.Is(err, store.ErrChannelMarkRollback) {
			return Result{}, artifactErr("POINTER_ROLLBACK_REJECTED",
				"the %s channel pointer for %s names version %s, which is no longer above the resolved-version high-water mark for that pair (marketplace/1 MKT-050)",
				prov.TrustChannel, m.ID, m.Version)
		}
		// MKT-093b: the required-pack floor, asserted in the same transaction and
		// therefore reachable from EVERY install path — a pointer resolution, an
		// explicit version pin, a direct upload, and a revert alike.
		if ferr := requiredFloorRefusal(err); ferr != err {
			return Result{}, ferr
		}
		// The revert's compare-and-swap: the row it meant to replace moved (or
		// vanished) before this transaction committed.
		if errors.Is(err, store.ErrPriorVersionChanged) {
			return Result{}, priorVersionChangedRefusal(m.ID, expectPriorVersion)
		}
		return Result{}, err
	}

	return Result{
		ID:          pack.ID,
		Version:     pack.Version,
		Pages:       pages,
		Collections: collectionNames(&m),
		Locales:     locales,
		Created:     created,
	}, nil
}

// collectBundleFiles resolves the manifest's declared pages and the artifact's
// locale catalogs into the PackFile set the store persists. Every ui.pages[]
// entry MUST resolve to a bundled document at its UIS-001 location (this is the
// page↔manifest path match), and every page document and locale catalog MUST
// parse as JSON — a missing document or non-JSON content is a typed
// ArtifactError, never a stored-then-broken pack. Nothing is executed: the
// documents are parsed only to prove they are well-formed JSON, then stored.
func (in *Installer) collectBundleFiles(m *manifest.PackManifest, bundle *Bundle) (files []store.PackFile, pages, locales []string, err error) {
	for _, p := range m.UI.Pages {
		entry := pageDocPath(p.Path)
		doc, ok := bundle.File(entry)
		if !ok {
			return nil, nil, nil, artifactErr("PACK_PAGE_DOC_MISSING",
				"ui.pages declares path %q but the artifact has no page document at %q (ui-schema/1 UIS-001)", p.Path, entry)
		}
		if !json.Valid(doc) {
			return nil, nil, nil, artifactErr("PACK_PAGE_DOC_INVALID",
				"page document %q is not valid JSON", entry)
		}
		files = append(files, store.PackFile{
			Kind: store.PackFilePage,
			Name: p.Path,
			Body: append(json.RawMessage(nil), doc...),
		})
		pages = append(pages, p.Path)
	}

	for _, name := range bundle.Names() {
		locale, ok := localeName(name)
		if !ok {
			continue
		}
		catalog, _ := bundle.File(name)
		if !json.Valid(catalog) {
			return nil, nil, nil, artifactErr("PACK_LOCALE_INVALID",
				"locale catalog %q is not valid JSON", name)
		}
		// MAN-110: a locale catalog is a FLAT {key: text} map. Prove it here — a
		// non-object top level or any non-string value is refused. The manifest
		// engine's validateLocale only checks messages/en.json is present, so
		// without this a pack could ship a catalog whose value is an object/array
		// under a manifest-referenced key (displayName/titleMsg); carried to the
		// console that non-string crashes React render. Defense-in-depth alongside
		// the renderer's own value-type guard. The catalog is parsed only to prove
		// its shape — nothing is executed.
		if badKey, ok := localeCatalogStrings(catalog); !ok {
			return nil, nil, nil, artifactErr("PACK_LOCALE_INVALID",
				"locale catalog %q must be a flat object of string values (MAN-110)%s", name, badKey)
		}
		files = append(files, store.PackFile{
			Kind: store.PackFileLocale,
			Name: locale,
			Body: append(json.RawMessage(nil), catalog...),
		})
		locales = append(locales, locale)
	}

	sort.Strings(pages)
	sort.Strings(locales)
	return files, pages, locales, nil
}

// localeCatalogStrings reports whether a locale catalog (already proven valid
// JSON) is a flat object whose every value is a string (MAN-110). On failure it
// also returns a `: "<key>" is not a string`-style suffix naming the first
// offending key (empty for a non-object top level), for the typed error's detail.
// It decodes only to inspect value shapes — it executes nothing.
func localeCatalogStrings(catalog []byte) (detail string, ok bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(catalog, &raw); err != nil {
		// Top level is not a JSON object (an array/string/number/null).
		return "", false
	}
	// Sort the keys so the reported offender is deterministic across runs.
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var s string
		if err := json.Unmarshal(raw[k], &s); err != nil {
			return fmt.Sprintf(": %q is not a string", k), false
		}
	}
	return "", true
}

// localeName reports whether a bundle entry names a locale catalog
// (messages/<locale>.json) and, if so, the locale. A nested path under
// messages/ (a "/" in the locale segment) is NOT a locale catalog.
func localeName(entry string) (string, bool) {
	if !strings.HasPrefix(entry, localePrefix) || !strings.HasSuffix(entry, localeExt) {
		return "", false
	}
	locale := entry[len(localePrefix) : len(entry)-len(localeExt)]
	if locale == "" || strings.Contains(locale, "/") {
		return "", false
	}
	return locale, true
}

// versionRegressionGuard is the store.InstallGuard that re-enforces MAN-053
// inside the install transaction: if the incoming dataModel.version is lower
// than the prior installed pack's, it refuses the install with the SAME typed
// *ManifestError the manifest engine's sequential check yields (so the api layer
// renders the identical 422 / DATAMODEL_VERSION_REGRESSION field error). It fires
// only when a prior row exists and only on an actual regression — the store runs
// it against the prior row read under its own write lock, closing the TOCTOU race
// the pipeline's pre-transaction installedVersion snapshot leaves open.
// pointerDowngradeGuard refuses a CHANNEL-POINTER resolution that would walk the
// INSTALLED pack backward, whatever channel it came from.
//
// MKT-050's mark is keyed (pack, trust channel), which leaves a downgrade path
// in which every individual step succeeds — exactly the shape MKT-094b's own
// rationale rejects. Install 2.0.0 from `verified`, then follow the `community`
// pointer at 1.0.0: that pair has no mark, so the per-channel check passes
// vacuously and 1.0.0 replaces the running 2.0.0. There is ONE installed pack
// row per id, so the floor that actually protects the box is the installed
// version, and it is re-read here inside the write transaction.
//
// Only pointer resolution is governed. An explicit version pin is the
// operator's own stated choice and is what MKT-044's archived reinstall needs.
func pointerDowngradeGuard(incomingVersion string) store.InstallGuard {
	return func(prior store.Pack) error {
		if prior.Version == incomingVersion || !VersionHigher(prior.Version, incomingVersion) {
			return nil
		}
		return artifactErr("POINTER_ROLLBACK_REJECTED",
			"the channel pointer names version %s, below the %s currently installed for %s (marketplace/1 MKT-050)",
			incomingVersion, prior.Version, prior.ID)
	}
}

func versionRegressionGuard(incomingVersion int) store.InstallGuard {
	return func(prior store.Pack) error {
		if incomingVersion < prior.DataModelVersion {
			return &ManifestError{Errors: []manifest.Error{{
				Code:  "DATAMODEL_VERSION_REGRESSION",
				Field: "dataModel.version",
				Message: fmt.Sprintf("dataModel.version %d is lower than the currently installed version %d (MAN-053)",
					incomingVersion, prior.DataModelVersion),
			}}}
		}
		return nil
	}
}

// verifyArtifactSignature runs packsig.VerifyBundle over the bundle's extracted
// entry set and maps its typed refusal onto this pipeline's stable
// ArtifactError codes (marketplace/1 Error taxonomy, rendered by the api layer
// as a 422 errors[] discriminant like every other PACK_* code):
//
//   - no envelope        → PACK_UNSIGNED (a stripped signature is refused
//     outright, never treated as a legacy or pre-verification artifact);
//   - malformed envelope, content/digest mismatch (a tampered payload or
//     manifest), or a signature that does not verify → PACK_SIGNATURE_INVALID;
//   - a key no trust anchor authorizes for the artifact's publisher namespace
//     → PACK_SIGNER_UNTRUSTED.
func verifyArtifactSignature(bundle *Bundle, anchors packsig.TrustAnchors) (packsig.Envelope, error) {
	files := make(map[string][]byte, len(bundle.Names()))
	for _, name := range bundle.Names() {
		body, _ := bundle.File(name)
		files[name] = body
	}
	env, err := packsig.VerifyBundle(files, anchors)
	if err == nil {
		return env, nil
	}
	verr, ok := err.(*packsig.VerifyError)
	if !ok {
		return packsig.Envelope{}, artifactErr("PACK_SIGNATURE_INVALID",
			"the artifact's signature could not be verified: %v", err)
	}
	switch verr.Reason {
	case packsig.ReasonUnsigned:
		return packsig.Envelope{}, artifactErr("PACK_UNSIGNED", "%s", verr.Message)
	case packsig.ReasonUntrusted:
		return packsig.Envelope{}, artifactErr("PACK_SIGNER_UNTRUSTED", "%s", verr.Message)
	default:
		return packsig.Envelope{}, artifactErr("PACK_SIGNATURE_INVALID", "%s", verr.Message)
	}
}

// checkResolvedIdentity enforces marketplace/1 MKT-040a: the resolved index
// entry's claimed (artifact_id, kind, subtype, version) must equal the signed
// identity of the artifact that entry pointed at. A mismatch refuses
// RESOLVED_IDENTITY_MISMATCH and installs neither identity.
//
// The failure this closes is not a corrupted download — the entry digest would
// already catch that. It is a source publishing entry `acme/b@2.0.0` pointing at
// a genuine, correctly-signed `acme/a@1.0.0`: every existing check passes, and
// what lands is a pack whose id and version differ from the one the reference,
// the install record, the channel pointer and the anti-rollback mark are all
// keyed on.
func checkResolvedIdentity(prov provenance, env packsig.Envelope) error {
	if prov.EntryID == env.ArtifactID && prov.EntryKind == env.Kind &&
		prov.EntrySubtype == env.Subtype && prov.EntryVersion == env.Version {
		return nil
	}
	return artifactErr("RESOLVED_IDENTITY_MISMATCH",
		"the resolved index entry claims %s (%s) version %s, but the artifact it names is signed as %s (%s) version %s",
		prov.EntryID, entryKindLabel(prov.EntryKind, prov.EntrySubtype), prov.EntryVersion,
		env.ArtifactID, entryKindLabel(env.Kind, env.Subtype), env.Version)
}

// entryKindLabel renders a kind/subtype pair for a refusal message.
func entryKindLabel(kind, subtype string) string {
	if subtype == "" {
		return kind
	}
	return kind + "/" + subtype
}

// installRecord builds the install record persisted with the pack
// (marketplace/1 MKT-094 + MKT-094a). ContentDigest, KeyID and VerifyingKey all
// come from the envelope that ACTUALLY verified — never from the reference, the
// entry, or anything the artifact merely asserts.
//
// VerifyingKey is what makes the record able to NAME the key rather than a label
// for it. This comment used to claim key_id alone meant "the record can only
// ever name a key that did vouch for these bytes"; that does not hold when two
// anchors share an id, which packsig permits on purpose so a colliding id cannot
// let a shadow anchor refuse the genuine publisher. The label was honest about
// the envelope and silent about which key behind it verified.
//
// A direct upload records SourceDirect with no trust channel and no entry
// digest: there is no channel pointer behind it to auto-track (MKT-090) and no
// index entry to name. The empty trust channel is the load-bearing part, not a
// missing value.
func installRecord(version string, env packsig.Envelope, prov *provenance) store.PackInstallRecord {
	rec := store.PackInstallRecord{
		ResolvedVersion: version,
		Source:          store.SourceDirect,
		ContentDigest:   env.ContentDigest,
		KeyID:           env.KeyID,
		VerifyingKey:    packsig.KeyDigest(env.VerifyingKey),
	}
	if prov != nil {
		rec.Source = prov.Source
		rec.TrustChannel = prov.TrustChannel
		rec.ArtifactDigest = prov.EntryDigest
		rec.StaleSource = prov.StaleSource
	}
	return rec
}

// collectionNames returns the pack's declared collection names (MAN-051), sorted
// — the summary the install Result reports and the pack-data surface will key on.
func collectionNames(m *manifest.PackManifest) []string {
	names := make([]string, 0, len(m.DataModel.Collections))
	for _, c := range m.DataModel.Collections {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}
