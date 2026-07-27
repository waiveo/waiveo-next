package archive

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// Tar entry names. manifest.json is the FIRST entry of the tar stream (ARC-030)
// — the ordering that makes a restore's pre-flight checks possible without
// streaming the whole archive first, and therefore what makes ARC-081's
// single-pass restore possible at all.
const (
	// ManifestEntryName is the manifest's tar entry name (ARC-030).
	ManifestEntryName = "manifest.json"
	// SnapshotEntryName is the workspace relational snapshot's tar entry name
	// (ARC-085). It carries no manifest-recorded hash of its own and does not
	// need one: it is always embedded in full inside the same encrypted body
	// ARC-024's digest recompute already covers, so a reader that passed that
	// check has verified these bytes exactly as thoroughly as ARC-062 verifies an
	// embedded asset's.
	SnapshotEntryName = "workspace.sqlite"
	// assetEntryPrefix precedes an embedded asset's hex hash (ARC-061).
	assetEntryPrefix = "assets/"
)

// Archive modes (ARC-031). Create emits ModeFull only.
const (
	ModeFull        = "full"
	ModeIncremental = "incremental"
)

// Asset storage dispositions (ARC-060).
const (
	// StorageEmbedded: the bytes ride in this container, as a tar entry at
	// assets/<hex> (ARC-061).
	StorageEmbedded = "embedded"
	// StorageByReference: the bytes are not carried here because the destination
	// already holds them or can obtain them independently (ARC-063).
	StorageByReference = "by-reference"
	// StorageInherited: the bytes are already present in this archive's base
	// archive (ARC-091). Only meaningful in incremental mode.
	StorageInherited = "inherited"
)

// assetRefPattern is `ctx/1`'s asset_ref grammar: `sha256:` followed by 64
// lowercase hex digits. It is enforced rather than assumed because ARC-061
// derives a tar entry NAME from the hash portion and ARC-062 compares a
// recomputed hash against it — both of which are meaningless if the reference is
// not actually a sha256 in canonical form.
var assetRefPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// PackLock is one entry of the pack lockfile (ARC-050): the pack, the exact
// version locked, the trust channel and registry source it was resolved from at
// export time, and the locked pack's own dataModel version.
//
// None of it is a trust assertion. A restore re-verifies every locked pack
// against the destination's CURRENT trust state (ARC-101) — `channel` is
// recorded so restore-time gating can tell a dev-channel lock from any other
// (ARC-052/103), not so the archive can vouch for itself.
type PackLock struct {
	PackID      string `json:"pack_id"`
	Version     string `json:"version"`
	Channel     string `json:"channel"`
	Source      string `json:"source"`
	SchemaEpoch int    `json:"schema_epoch"`
}

// AssetEntry is one row of the manifest's asset table (ARC-060).
type AssetEntry struct {
	AssetRef string `json:"asset_ref"`
	Size     int64  `json:"size"`
	// ContentType is informative only — no requirement in archive/1 conditions
	// behavior on its value (ARC-065), so nothing here branches on it.
	ContentType string `json:"content_type,omitempty"`
	Storage     string `json:"storage"`
}

// SecretStub is one wrapped, opaque secret value (ARC-070). This package defines
// no shape for WrappedValue and performs no operation on its contents beyond
// carrying it unchanged, byte for byte, from source workspace to destination —
// ARC-072 forbids any step of an export or restore from computing an individual
// secret's plaintext.
type SecretStub struct {
	StubID       string `json:"stub_id"`
	WrappedValue string `json:"wrapped_value"`
}

// DataKeyWrap carries the source workspace's own data key, re-wrapped under the
// sub-key ARC-011 derives for that purpose (ARC-071) — never the raw data key,
// and never the stubs re-wrapped one by one. That single re-wrap is what makes
// the entire secret_stubs array portable as one unit without the export ever
// touching an individual secret's value.
type DataKeyWrap struct {
	WrappedValue string `json:"wrapped_value"`
}

// BaseArchiveRef identifies the prior archive an incremental archive deltas
// against, by that archive's own outer-header digest (ARC-090).
//
// Incremental archives are not implemented here (see the package doc). This type
// exists so an incremental manifest survives a decode/encode round trip with the
// field intact instead of being quietly dropped — the failure mode that would
// make adding incremental support later a bug hunt rather than a feature.
type BaseArchiveRef struct {
	Digest    string `json:"digest"`
	CreatedAt int64  `json:"created_at"`
}

// Manifest is the container's first entry (ARC-030/031): a JSON document
// describing everything else the container holds, self-sufficient to validate
// before any bulk content is read.
//
// Field order here is the marshal order and matches the contract's "Wire shapes"
// block.
type Manifest struct {
	CreatedAt           int64           `json:"created_at"`
	Mode                string          `json:"mode"`
	BaseArchive         *BaseArchiveRef `json:"base_archive,omitempty"`
	WorkspaceID         string          `json:"workspace_id"`
	PlatformSchemaEpoch int             `json:"platform_schema_epoch"`
	Packs               []PackLock      `json:"packs"`
	Assets              []AssetEntry    `json:"assets"`
	SecretStubs         []SecretStub    `json:"secret_stubs"`
	DataKeyWrap         DataKeyWrap     `json:"data_key_wrap"`
}

// requiredManifestFields is ARC-031's required-field set for a full-mode
// manifest. Presence is checked against the RAW document rather than against the
// decoded struct because encoding/json cannot tell an absent `packs` from a
// present `"packs": []` — and ARC-031 requires the field, while an empty pack
// set is a legitimate workspace.
var requiredManifestFields = []string{
	"created_at", "mode", "workspace_id", "platform_schema_epoch",
	"packs", "assets", "secret_stubs", "data_key_wrap",
}

// parseManifest decodes and validates a manifest document (ARC-031/033).
//
// Unknown fields are TOLERATED, both at the top level and inside ARC-031's
// objects (ARC-032): an unrecognized field is forward-compatible minor-version
// growth (ARC-004), not a validation failure. encoding/json's default
// ignore-unknown behavior is that tolerance — DisallowUnknownFields would be a
// direct contract violation here, which is why it is deliberately absent.
//
// Every refusal is MANIFEST_INVALID (ARC-033), and every one of them happens
// before a single asset or the workspace snapshot has been handed to a caller
// (ARC-082).
func parseManifest(raw []byte) (Manifest, error) {
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		return Manifest{}, wrapf(CodeManifestInvalid, err, "manifest is not a JSON object")
	}
	for _, f := range requiredManifestFields {
		v, ok := present[f]
		if !ok {
			return Manifest{}, codedf(CodeManifestInvalid, "manifest is missing the required field %q", f)
		}
		if string(v) == "null" {
			return Manifest{}, codedf(CodeManifestInvalid, "manifest field %q is null", f)
		}
	}

	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, wrapf(CodeManifestInvalid, err, "manifest did not decode")
	}
	if err := validateManifest(m, present); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// validateManifest applies every semantic rule in the contract's Manifest group
// to an already-decoded manifest. present is the raw field map, consulted only
// for the incremental-mode `base_archive` requirement, whose presence (rather
// than its decoded value) is what ARC-031 conditions on.
func validateManifest(m Manifest, present map[string]json.RawMessage) error {
	if m.CreatedAt <= 0 {
		return codedf(CodeManifestInvalid, "created_at (%d) is not a positive epoch-millisecond Timestamp", m.CreatedAt)
	}

	switch m.Mode {
	case ModeFull:
		// A full archive is self-sufficient and references no other archive
		// (Definitions), so a base_archive here is a contradiction, not an
		// additive unknown field.
		if _, ok := present["base_archive"]; ok {
			return codedf(CodeManifestInvalid, "a full-mode manifest carries base_archive, which only an incremental archive may")
		}
	case ModeIncremental:
		if _, ok := present["base_archive"]; !ok {
			return codedf(CodeManifestInvalid, "an incremental-mode manifest is missing the required field \"base_archive\"")
		}
		if m.BaseArchive == nil || m.BaseArchive.Digest == "" {
			return codedf(CodeManifestInvalid, "base_archive.digest is empty")
		}
	default:
		return codedf(CodeManifestInvalid, "mode %q is neither %q nor %q", m.Mode, ModeFull, ModeIncremental)
	}

	if !ulid.Valid(m.WorkspaceID) {
		return codedf(CodeManifestInvalid, "workspace_id %q is not a ULID", m.WorkspaceID)
	}
	// ARC-040: set at export time to the source workspace's own platform schema
	// epoch, and a positive integer. Whether the DESTINATION can open that epoch
	// is a different question on a different axis (ARC-041/104, EPOCH_TOO_NEW),
	// decided by a restorer against its own understanding — never here.
	if m.PlatformSchemaEpoch <= 0 {
		return codedf(CodeManifestInvalid, "platform_schema_epoch (%d) is not a positive integer", m.PlatformSchemaEpoch)
	}

	seenPack := make(map[string]bool, len(m.Packs))
	for i, p := range m.Packs {
		switch {
		case p.PackID == "":
			return codedf(CodeManifestInvalid, "packs[%d] has an empty pack_id", i)
		case p.Version == "":
			return codedf(CodeManifestInvalid, "packs[%d] (%s) has an empty version", i, p.PackID)
		case p.Channel == "":
			// ARC-052: channel must distinguish a dev-channel lock from any
			// other, so restore-time gating (ARC-103) has something to gate on.
			// An empty channel gates as nothing.
			return codedf(CodeManifestInvalid, "packs[%d] (%s) has an empty channel", i, p.PackID)
		case p.Source == "":
			return codedf(CodeManifestInvalid, "packs[%d] (%s) has an empty source", i, p.PackID)
		case p.SchemaEpoch <= 0:
			return codedf(CodeManifestInvalid, "packs[%d] (%s) has a non-positive schema_epoch (%d)", i, p.PackID, p.SchemaEpoch)
		}
		// ARC-051: a workspace locks at most one version of any given pack.
		if seenPack[p.PackID] {
			return codedf(CodeManifestInvalid, "packs contains %s more than once", p.PackID)
		}
		seenPack[p.PackID] = true
	}

	seenAsset := make(map[string]bool, len(m.Assets))
	for i, a := range m.Assets {
		if !assetRefPattern.MatchString(a.AssetRef) {
			return codedf(CodeManifestInvalid, "assets[%d] asset_ref %q is not a sha256: URI with 64 lowercase hex digits", i, a.AssetRef)
		}
		if a.Size < 0 {
			return codedf(CodeManifestInvalid, "assets[%d] (%s) has a negative size (%d)", i, a.AssetRef, a.Size)
		}
		switch a.Storage {
		case StorageEmbedded, StorageByReference:
		case StorageInherited:
			// ARC-091: `inherited` means "already present in the base archive",
			// which a full archive by definition does not have.
			if m.Mode != ModeIncremental {
				return codedf(CodeManifestInvalid, "assets[%d] (%s) is %q, which only an incremental archive may declare", i, a.AssetRef, StorageInherited)
			}
		default:
			return codedf(CodeManifestInvalid, "assets[%d] (%s) has storage %q, not one of %q/%q/%q",
				i, a.AssetRef, a.Storage, StorageEmbedded, StorageByReference, StorageInherited)
		}
		if seenAsset[a.AssetRef] {
			return codedf(CodeManifestInvalid, "assets contains %s more than once", a.AssetRef)
		}
		seenAsset[a.AssetRef] = true
	}

	for i, s := range m.SecretStubs {
		if !ulid.Valid(s.StubID) {
			return codedf(CodeManifestInvalid, "secret_stubs[%d] stub_id %q is not a ULID", i, s.StubID)
		}
		// wrapped_value is opaque (ARC-070) — its emptiness is the only thing
		// checkable without interpreting it, and an empty wrap is not a secret.
		if s.WrappedValue == "" {
			return codedf(CodeManifestInvalid, "secret_stubs[%d] (%s) has an empty wrapped_value", i, s.StubID)
		}
	}

	if m.DataKeyWrap.WrappedValue == "" {
		return codedf(CodeManifestInvalid, "data_key_wrap.wrapped_value is empty")
	}
	return nil
}

// assetEntryName is ARC-061's tar entry name for an embedded asset:
// `assets/<hex>`, where <hex> is asset_ref's hash portion with its `sha256:`
// prefix stripped. It assumes assetRefPattern has already matched.
func assetEntryName(assetRef string) string {
	return assetEntryPrefix + assetRef[len("sha256:"):]
}

// assetRefFromEntryName is assetEntryName's inverse, reporting ok=false for a
// name that is not a well-formed assets/<hex> entry.
func assetRefFromEntryName(name string) (string, bool) {
	if len(name) <= len(assetEntryPrefix) || name[:len(assetEntryPrefix)] != assetEntryPrefix {
		return "", false
	}
	ref := "sha256:" + name[len(assetEntryPrefix):]
	if !assetRefPattern.MatchString(ref) {
		return "", false
	}
	return ref, true
}

// String is a diagnostic rendering, never a wire form.
func (m Manifest) String() string {
	return fmt.Sprintf("archive manifest{mode:%s workspace:%s epoch:%d packs:%d assets:%d stubs:%d}",
		m.Mode, m.WorkspaceID, m.PlatformSchemaEpoch, len(m.Packs), len(m.Assets), len(m.SecretStubs))
}
