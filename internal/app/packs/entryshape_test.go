package packs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// entryshape_test.go pins the per-entry rules a resolved index entry passes
// before its artifact is fetched. Five of the six were held by nothing.
//
// They divide into two kinds, and the distinction is the interesting part:
//
//   - rules about what this entry IS (kind, version) — a well-formed index can
//     legitimately carry entries this pipeline does not install, and refusing
//     them is not an error report about the registry.
//   - rules about shapes this deployment DOES NOT HANDLE (split, transport
//     compression). The code's own words: refused, never mishandled. A split or
//     compressed object fetched as if it were a plain artifact is a wrong thing
//     handed to the verifier, and the verifier's refusal would describe a
//     corrupt artifact rather than an unsupported publication.

// resolveInstall runs one InstallRef against a registry holding a single
// published entry whose members `extra` overrides.
func resolveInstall(t *testing.T, extra map[string]any) error {
	t.Helper()
	st := openStore(t)
	reg := newRegistry(t, "fixture")
	src := reg.source()
	in, signer := marketInstaller(t, st, src)

	reg.publish("acme/menu-board", "1.0.0", versionedPack(t, signer, "1.0.0"), extra)
	reg.point("acme/menu-board", "community", "1.0.0")
	reg.reindex()

	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	return err
}

// TestEntryOfAnotherKindIsRefusedDistinctlyFromAnUnknownKind pins the two kind
// rules, which are adjacent and different.
//
// An UNKNOWN kind is a malformed index: MKT-041 refuses it PACK_KIND_UNKNOWN. A
// KNOWN kind that simply is not a pack is a perfectly good entry this pipeline
// does not install, and it answers MARKETPLACE_REF_UNRESOLVED. Collapsing the
// two would tell an operator their registry is broken when it is merely serving
// something else as well.
func TestEntryOfAnotherKindIsRefusedDistinctlyFromAnUnknownKind(t *testing.T) {
	err := resolveInstall(t, map[string]any{"kind": "content-package"})
	if err == nil {
		t.Fatal("a content-package entry was installed by the pack pipeline")
	}
	if code := refusalCode(t, err); code != "MARKETPLACE_REF_UNRESOLVED" {
		t.Errorf("a KNOWN non-pack kind refused with %s, want MARKETPLACE_REF_UNRESOLVED — it is not a malformed "+
			"index, it is an entry this pipeline does not install", code)
	}

	// The neighbouring rule, for contrast: an unknown kind IS a malformed index.
	err = resolveInstall(t, map[string]any{"kind": "not-a-kind"})
	if code := refusalCode(t, err); code != "PACK_KIND_UNKNOWN" {
		t.Errorf("an UNKNOWN kind refused with %s, want PACK_KIND_UNKNOWN (MKT-041)", code)
	}
}

// TestAPinnedVersionOutsideTheGrammarIsRefusedBeforeResolution pins the rule
// that actually catches a malformed version, and records why the one below it
// cannot be pinned.
//
// selectEntry matches an entry's version EXACTLY, so by the time the entry-level
// ValidVersion check runs, the version came from one of two places and both have
// already validated it: a channel pointer is checked in resolveFrom, and an
// explicit pin is checked at the Ref before resolution even begins. The
// entry-level check is therefore a backstop no input reaches — the same category
// as isJSONObject's null pre-check in internal/events and readCapped's byte cap
// in reader.go.
//
// My first version of this test asserted the phrase "MAJOR.MINOR.PATCH" and
// passed while the entry-level guard was deleted, because the Ref-level refusal
// carries the same phrase. The two are told apart by their CODE:
// MARKETPLACE_REF_INVALID is the caller's reference being malformed;
// MARKETPLACE_REF_UNRESOLVED is a well-formed reference that resolved to
// nothing usable.
func TestAPinnedVersionOutsideTheGrammarIsRefusedBeforeResolution(t *testing.T) {
	st := openStore(t)
	reg := newRegistry(t, "fixture")
	src := reg.source()
	in, signer := marketInstaller(t, st, src)

	// The registry even publishes an entry AT that malformed version, so the
	// refusal cannot be "no such entry" — the reference itself is what is bad.
	reg.publish("acme/menu-board", "1.0", versionedPack(t, signer, "1.0.0"), map[string]any{"version": "1.0"})
	reg.reindex()

	_, err := in.InstallRef(context.Background(), packs.Ref{
		PackID: "acme/menu-board", TrustChannel: "community", Version: "1.0",
	})
	if err == nil {
		t.Fatal("a pinned two-component version resolved")
	}
	if code := refusalCode(t, err); code != "MARKETPLACE_REF_INVALID" {
		t.Errorf("refused with %s, want MARKETPLACE_REF_INVALID — the caller's reference is malformed, which is a "+
			"different answer from a good reference that resolved to nothing", code)
	}
	if !strings.Contains(err.Error(), "MAJOR.MINOR.PATCH") {
		t.Errorf("refusal %q does not name the grammar", err)
	}
}

// TestUnsupportedPublicationShapesAreRefusedNotMishandled pins the split and
// compressed cases.
//
// Both need the bounded streaming decompressor CHI-028 requires, which this
// deployment does not build. Fetching either as if it were a plain artifact
// hands the verifier a fragment or a compressed blob — it would refuse, but as a
// CORRUPT ARTIFACT, telling an operator their publisher signed something broken
// rather than that this host cannot install that publication shape.
func TestUnsupportedPublicationShapesAreRefusedNotMishandled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extra   map[string]any
		wantMsg string
	}{
		{"split across parts (CHI-024)", map[string]any{"parts": []any{
			map[string]any{"digest": "sha256:aa", "size": 10},
		}}, "split"},
		{"transport-compressed (CHI-026)", map[string]any{"compression": "zstd"}, "compression"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := resolveInstall(t, tc.extra)
			if err == nil {
				t.Fatalf("an entry published %s resolved", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refused with %q, want one naming the unsupported shape — a refusal describing a corrupt "+
					"artifact sends an operator to their publisher instead of to this host's limits", err)
			}
		})
	}
}

// TestEntryMissingItsFetchTripleIsRefused pins CHI-020's minimum: a digest that
// names its algorithm, a positive size, and somewhere to fetch from. Each is
// separately absent-able and each makes the entry unusable, so the rule is
// driven member by member rather than once.
func TestEntryMissingItsFetchTripleIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]any
	}{
		{"a digest with no algorithm prefix", map[string]any{"digest": "deadbeef"}},
		{"a zero size", map[string]any{"size": 0}},
		{"a negative size", map[string]any{"size": -1}},
		{"no download url", map[string]any{"download_url": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := resolveInstall(t, tc.extra)
			if err == nil {
				t.Fatalf("an entry with %s resolved", tc.name)
			}
			if !strings.Contains(err.Error(), "CHI-020") {
				t.Errorf("refused with %q, want the CHI-020 entry-shape rule", err)
			}
		})
	}

	// The control: the untouched entry resolves, so every case above is the rule
	// it names rather than a fixture that never worked.
	if err := resolveInstall(t, nil); err != nil {
		t.Errorf("a conformant entry was refused: %v", err)
	}
}
