package packs_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// indexheader_test.go pins the two checks a registry index passes before any of
// its entries are read: the format major this client implements, and that the
// index names the channel its source is configured for.
//
// Both were pinned by nothing. Both are refusals about the DOCUMENT rather than
// about any artifact in it, so they run before an entry is selected — which is
// also why an entry-level test cannot reach them.

// patchSigned rewrites a member of the already-written index document's `signed`
// block, so a case can serve a header the fixture would never produce.
func patchSigned(t *testing.T, dir string, member string, value any) {
	t.Helper()
	path := filepath.Join(dir, "index.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse index: %v", err)
	}
	signed, ok := doc["signed"].(map[string]any)
	if !ok {
		t.Fatalf("index has no signed block: %s", raw)
	}
	signed[member] = value
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

// TestIndexServingAnotherChannelIsRefused pins the channel agreement between a
// source's configuration and the document it serves.
//
// A trust channel is not a label on an artifact — it is the posture a deployment
// has agreed to for a whole source. An index that names a channel other than the
// one its source was configured for is either misconfigured or is presenting
// artifacts under a posture the operator never consented to, and the resolver
// cannot tell which. It refuses either way, before selecting an entry.
func TestIndexServingAnotherChannelIsRefused(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	reg := newRegistry(t, "fixture")
	src := reg.source()
	in, signer := marketInstaller(t, st, src)
	publishVersion(t, reg, signer, "1.0.0")

	// The index now claims a different channel than the source is configured
	// for. Written AFTER publishVersion, which rewrites the document.
	patchSigned(t, reg.dir, "channel", "marketplace/first-party")

	_, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if err == nil {
		t.Fatal("an index naming a channel other than its source's resolved")
	}
	if code := refusalCode(t, err); code != "MARKETPLACE_REF_UNRESOLVED" {
		t.Fatalf("refused with %s, want MARKETPLACE_REF_UNRESOLVED", code)
	}
	if !containsAll(err.Error(), "channel", "first-party") {
		t.Errorf("refusal %q does not name the channel disagreement — an operator cannot tell this from any "+
			"other unresolved reference", err)
	}
}

// TestIndexOfAnUnimplementedFormatMajorIsRefused pins CHI-090. The reason is in
// the code's own comment: refuse a major this client does not implement "rather
// than parsing a schema it does not understand". A future format may move or
// repurpose a member this client reads, so a best-effort parse of an unknown
// major is how a resolver silently misreads a document rather than failing.
func TestIndexOfAnUnimplementedFormatMajorIsRefused(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	reg := newRegistry(t, "fixture")
	src := reg.source()
	in, signer := marketInstaller(t, st, src)
	publishVersion(t, reg, signer, "1.0.0")

	patchSigned(t, reg.dir, "format_version", "2.0")

	_, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if err == nil {
		t.Fatal("an index declaring format_version 2.0 resolved")
	}
	if !containsAll(err.Error(), "format_version", "2.0") {
		t.Errorf("refusal %q does not name the format version — the refusal reads like an ordinary unresolved "+
			"reference rather than a client that is too old", err)
	}

	// The control: restored to a major this client implements, the same index
	// resolves — so the rule is the major and not the patching.
	patchSigned(t, reg.dir, "format_version", "1.4")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Errorf("format_version 1.4 was refused: %v — CHI-090 gates on the MAJOR, so a later minor of a "+
			"format this client implements must still resolve", err)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
