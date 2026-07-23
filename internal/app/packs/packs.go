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
package packs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/manifest"
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
// store and the artifact-safety Limits; the host registries are rebuilt per
// install (their BundleFiles and InstalledDataModelVersion are per-artifact).
type Installer struct {
	store  *store.Store
	limits Limits
}

// NewInstaller builds an Installer over st with the default artifact limits.
func NewInstaller(st *store.Store) *Installer {
	return &Installer{store: st, limits: DefaultLimits}
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
// atomically, gated by the real manifest engine. It returns a Result on success,
// an *ArtifactError for an unsafe/malformed artifact, or a *ManifestError for a
// manifest the engine refused. It executes NOTHING from the artifact.
func (in *Installer) Install(ctx context.Context, artifact []byte) (Result, error) {
	bundle, err := ReadBundle(artifact, in.limits)
	if err != nil {
		return Result{}, err
	}

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
		ID:               m.ID,
		Version:          m.Version,
		DataModelVersion: m.DataModel.Version,
		Manifest:         append(json.RawMessage(nil), manifestBytes...),
		Files:            files,
	}
	pack, created, err := in.store.InstallPack(ctx, spec)
	if err != nil {
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
