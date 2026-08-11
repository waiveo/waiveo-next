package archive

import (
	"encoding/json"
	"testing"
)

// validManifestJSON is the contract's own "Wire shapes" full-mode manifest,
// trimmed to this suite's fixture ids. Every case below is this document with
// one thing changed, so a failure names exactly the rule that fired.
const validManifestJSON = `{
  "created_at": 1752537600000,
  "mode": "full",
  "workspace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZC",
  "platform_schema_epoch": 4,
  "packs": [
    { "pack_id": "waiveo/slidecast", "version": "2.2.0", "channel": "first-party", "source": "https://index.example/waiveo", "schema_epoch": 3 }
  ],
  "assets": [
    { "asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "size": 20481, "content_type": "image/png", "storage": "embedded" }
  ],
  "secret_stubs": [
    { "stub_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZD", "wrapped_value": "AQIDBAUGBwgJCgsMDQ4PEA" }
  ],
  "data_key_wrap": { "wrapped_value": "EBAPDg0MCwoJCAcGBQQDAgE" }
}`

// mutateManifest returns validManifestJSON with a top-level key replaced (or,
// with a nil value, deleted).
func mutateManifest(t *testing.T, key string, value any) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(validManifestJSON), &m); err != nil {
		t.Fatalf("decode the valid manifest fixture: %v", err)
	}
	if value == nil {
		delete(m, key)
	} else {
		m[key] = value
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal mutated manifest: %v", err)
	}
	return b
}

// TestParseManifestAcceptsAValidDocument confirms the fixture every negative
// case is derived from is itself accepted — without which those cases would
// prove nothing.
func TestParseManifestAcceptsAValidDocument(t *testing.T) {
	m, err := parseManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatalf("parseManifest() on the contract's own wire shape = %v, want nil", err)
	}
	if m.Mode != ModeFull {
		t.Errorf("Mode = %q, want %q", m.Mode, ModeFull)
	}
	if m.PlatformSchemaEpoch != 4 {
		t.Errorf("PlatformSchemaEpoch = %d, want 4", m.PlatformSchemaEpoch)
	}
	if len(m.Packs) != 1 || m.Packs[0].PackID != "waiveo/slidecast" {
		t.Errorf("Packs = %+v, want the single fixture lock", m.Packs)
	}
}

// TestParseManifestToleratesUnknownFields is ARC-032: a reader MUST tolerate an
// unrecognized top-level field, or an unrecognized optional field inside one of
// ARC-031's objects, treating it as forward-compatible minor-version growth (ARC-004)
// rather than a validation failure. Refusing one would make every additive
// field a breaking change.
func TestParseManifestToleratesUnknownFields(t *testing.T) {
	tests := map[string][]byte{
		"an unknown top-level field": mutateManifest(t, "retention_policy", "keep-forever"),
		"an unknown field inside a pack": mutateManifest(t, "packs", []any{map[string]any{
			"pack_id": "waiveo/slidecast", "version": "2.2.0", "channel": "first-party",
			"source": "https://index.example/waiveo", "schema_epoch": 3,
			"installed_by": "a field a future minor added",
		}}),
		"an unknown field inside an asset": mutateManifest(t, "assets", []any{map[string]any{
			"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"size":      20481, "storage": "embedded",
			"thumbnail_ref": "a field a future minor added",
		}}),
		"an unknown field inside a secret stub": mutateManifest(t, "secret_stubs", []any{map[string]any{
			"stub_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZD", "wrapped_value": "AQIDBAUGBwgJCgsMDQ4PEA",
			"rotated_at": 1752537600000,
		}}),
	}

	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseManifest(doc); err != nil {
				t.Fatalf("parseManifest() with %s = %v, want nil (ARC-032)", name, err)
			}
		})
	}
}

// TestParseManifestRefusesInvalidShapes is ARC-033: a manifest failing
// ARC-031's required-field shape, or any other rule in the Manifest group,
// refuses with MANIFEST_INVALID — before any asset or the workspace snapshot has
// been written anywhere.
func TestParseManifestRefusesInvalidShapes(t *testing.T) {
	tests := map[string][]byte{
		// ARC-031 required fields.
		"missing created_at":            mutateManifest(t, "created_at", nil),
		"missing mode":                  mutateManifest(t, "mode", nil),
		"missing workspace_id":          mutateManifest(t, "workspace_id", nil),
		"missing platform_schema_epoch": mutateManifest(t, "platform_schema_epoch", nil),
		"missing packs":                 mutateManifest(t, "packs", nil),
		"missing assets":                mutateManifest(t, "assets", nil),
		"missing secret_stubs":          mutateManifest(t, "secret_stubs", nil),
		"missing data_key_wrap":         mutateManifest(t, "data_key_wrap", nil),
		"null packs":                    []byte(`{"created_at":1,"mode":"full","workspace_id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZC","platform_schema_epoch":1,"packs":null,"assets":[],"secret_stubs":[],"data_key_wrap":{"wrapped_value":"x"}}`),

		// Not an object at all.
		"a JSON array":  []byte(`[]`),
		"a JSON string": []byte(`"manifest"`),
		"not JSON":      []byte(`{`),

		// ARC-031 mode.
		"an unknown mode": mutateManifest(t, "mode", "differential"),
		"an empty mode":   mutateManifest(t, "mode", ""),
		// A full archive is self-sufficient and references no other archive.
		"a full archive with base_archive": mutateManifest(t, "base_archive",
			map[string]any{"digest": "b5d4045c", "created_at": 1752537600000}),

		// ARC-031 identity / ARC-040 epoch.
		"a non-ULID workspace_id":          mutateManifest(t, "workspace_id", "workspace-1"),
		"a zero platform_schema_epoch":     mutateManifest(t, "platform_schema_epoch", 0),
		"a negative platform_schema_epoch": mutateManifest(t, "platform_schema_epoch", -4),
		"a zero created_at":                mutateManifest(t, "created_at", 0),

		// ARC-050/051/052 pack lockfile.
		"a pack with no pack_id": mutateManifest(t, "packs", []any{map[string]any{
			"version": "2.2.0", "channel": "first-party", "source": "s", "schema_epoch": 3,
		}}),
		"a pack with no channel": mutateManifest(t, "packs", []any{map[string]any{
			"pack_id": "waiveo/slidecast", "version": "2.2.0", "source": "s", "schema_epoch": 3,
		}}),
		"a pack with no source": mutateManifest(t, "packs", []any{map[string]any{
			"pack_id": "waiveo/slidecast", "version": "2.2.0", "channel": "first-party", "schema_epoch": 3,
		}}),
		"a pack with no schema_epoch": mutateManifest(t, "packs", []any{map[string]any{
			"pack_id": "waiveo/slidecast", "version": "2.2.0", "channel": "first-party", "source": "s",
		}}),
		"the same pack_id twice": mutateManifest(t, "packs", []any{
			map[string]any{"pack_id": "waiveo/slidecast", "version": "2.2.0", "channel": "first-party", "source": "s", "schema_epoch": 3},
			map[string]any{"pack_id": "waiveo/slidecast", "version": "2.3.0", "channel": "first-party", "source": "s", "schema_epoch": 3},
		}),

		// ARC-060 asset references.
		"an asset_ref that is not a sha256 URI": mutateManifest(t, "assets", []any{map[string]any{
			"asset_ref": "md5:abc", "size": 1, "storage": "embedded",
		}}),
		"an asset_ref with uppercase hex": mutateManifest(t, "assets", []any{map[string]any{
			"asset_ref": "sha256:E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855", "size": 1, "storage": "embedded",
		}}),
		"an asset with an unknown storage": mutateManifest(t, "assets", []any{map[string]any{
			"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "size": 1, "storage": "external",
		}}),
		"a full archive with an inherited asset": mutateManifest(t, "assets", []any{map[string]any{
			"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "size": 1, "storage": "inherited",
		}}),
		"a negative asset size": mutateManifest(t, "assets", []any{map[string]any{
			"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "size": -1, "storage": "embedded",
		}}),

		// ARC-070/071 secrets.
		"a stub with a non-ULID stub_id": mutateManifest(t, "secret_stubs", []any{map[string]any{
			"stub_id": "stub-1", "wrapped_value": "AQID",
		}}),
		"a stub with an empty wrapped_value": mutateManifest(t, "secret_stubs", []any{map[string]any{
			"stub_id": "01J8Z3K4N5P6Q7R8S9T0V1W2ZD", "wrapped_value": "",
		}}),
		"an empty data_key_wrap": mutateManifest(t, "data_key_wrap", map[string]any{"wrapped_value": ""}),
	}

	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseManifest(doc)
			if got := Code(err); got != CodeManifestInvalid {
				t.Fatalf("parseManifest() with %s = %v (code %q), want code %q", name, err, got, CodeManifestInvalid)
			}
		})
	}
}

// TestParseManifestIncrementalShape covers the one thing this package does for
// incremental archives: carry their shape honestly. ARC-031 requires
// `base_archive` on an incremental manifest and ARC-091 allows `inherited`
// assets only there. Resolving a base-archive chain (ARC-092/094) is a
// restorer's job and is deliberately not implemented here — but the manifest
// must still survive a decode intact, or adding that later starts with a bug.
func TestParseManifestIncrementalShape(t *testing.T) {
	base := map[string]any{"digest": "b5d4045c3f466fa91fe2cc6abe79232a1a57cdf104f7a26e716e0a1e2789df78", "created_at": 1752537600000}

	t.Run("base_archive round-trips", func(t *testing.T) {
		var m map[string]any
		if err := json.Unmarshal([]byte(validManifestJSON), &m); err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		m["mode"] = ModeIncremental
		m["base_archive"] = base
		m["assets"] = []any{map[string]any{
			"asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"size":      20481, "storage": "inherited",
		}}
		doc, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		got, err := parseManifest(doc)
		if err != nil {
			t.Fatalf("parseManifest() on an incremental manifest = %v, want nil", err)
		}
		if got.BaseArchive == nil {
			t.Fatal("BaseArchive is nil — an incremental manifest lost its base_archive on decode (ARC-090)")
		}
		if got.BaseArchive.Digest != base["digest"] {
			t.Errorf("BaseArchive.Digest = %q, want %q", got.BaseArchive.Digest, base["digest"])
		}
		if got.BaseArchive.CreatedAt != int64(1752537600000) {
			t.Errorf("BaseArchive.CreatedAt = %d, want 1752537600000", got.BaseArchive.CreatedAt)
		}
	})

	t.Run("an incremental manifest with no base_archive", func(t *testing.T) {
		doc := mutateManifest(t, "mode", ModeIncremental)
		_, err := parseManifest(doc)
		if got := Code(err); got != CodeManifestInvalid {
			t.Fatalf("parseManifest() = %v (code %q), want code %q (ARC-031)", err, got, CodeManifestInvalid)
		}
	})
}

// TestAssetEntryNameRoundTrip pins ARC-061's naming rule in both directions:
// `assets/<hex>` is asset_ref's hash portion with the `sha256:` prefix stripped,
// and nothing else parses as an asset entry.
func TestAssetEntryNameRoundTrip(t *testing.T) {
	const ref = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	const want = "assets/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	if got := assetEntryName(ref); got != want {
		t.Errorf("assetEntryName(%q) = %q, want %q", ref, got, want)
	}
	got, ok := AssetRefFromEntryName(want)
	if !ok || got != ref {
		t.Errorf("AssetRefFromEntryName(%q) = (%q, %v), want (%q, true)", want, got, ok, ref)
	}

	rejected := []string{
		"assets/", "assets", "workspace.sqlite", "manifest.json",
		"assets/not-hex", "assets/9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08",
		"nested/assets/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"assets/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08/extra",
	}
	for _, name := range rejected {
		if _, ok := AssetRefFromEntryName(name); ok {
			t.Errorf("AssetRefFromEntryName(%q) = ok, want not ok", name)
		}
	}
}
