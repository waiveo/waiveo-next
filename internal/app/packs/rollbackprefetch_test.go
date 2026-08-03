package packs_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// countingFetcher records which URLs were retrieved, so a test can assert that a
// refusal happened BEFORE a download rather than after one.
type countingFetcher struct {
	inner packs.Fetcher
	mu    sync.Mutex
	urls  []string
}

func (f *countingFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	f.mu.Lock()
	f.urls = append(f.urls, rawURL)
	f.mu.Unlock()
	return f.inner.Fetch(ctx, rawURL)
}

// artifactFetches returns the retrievals that were NOT the index document — the
// artifact downloads a refused resolution must never perform.
func (f *countingFetcher) artifactFetches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, u := range f.urls {
		if !strings.Contains(u, "index") {
			out = append(out, u)
		}
	}
	return out
}

// TestPointerRollbackIsRefusedBeforeTheArtifactIsFetched pins MKT-050's
// resolution-time anti-rollback, and specifically the property that makes it
// distinct from the in-transaction check that shares its error code.
//
// Both refuse a pointer walking a channel backward, and both answer
// POINTER_ROLLBACK_REJECTED — so a test asserting the code passes whichever one
// fired, and deleting the resolution-time check is invisible. That is exactly
// what a mutation sweep found.
//
// The resolution-time check earns its place by refusing BEFORE the download, as
// its own comment says: "a refused resolution should not even download". Without
// it, a registry that walks its pointer backward makes every client fetch and
// verify an artifact it will then throw away — work an untrusted party controls
// the size of. So the observable that distinguishes the two is whether the
// artifact was fetched at all.
func TestPointerRollbackIsRefusedBeforeTheArtifactIsFetched(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	reg := newRegistry(t, "fixture")
	src := reg.source()

	counter := &countingFetcher{inner: src.Fetcher}
	src.Fetcher = counter
	in, signer := marketInstaller(t, st, src)

	// Resolve 2.0.0 through the channel pointer, which raises the high-water
	// mark for (pack, channel).
	publishVersion(t, reg, signer, "2.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("install 2.0.0 through the pointer: %v", err)
	}

	// The registry now walks the pointer BACKWARD to 1.0.0 — the compromised or
	// mistaken registry MKT-050 exists for.
	publishVersion(t, reg, signer, "1.0.0")

	before := len(counter.artifactFetches())
	_, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if err == nil {
		t.Fatal("a channel pointer naming a version below the high-water mark resolved (MKT-050)")
	}
	if code := refusalCode(t, err); code != "POINTER_ROLLBACK_REJECTED" {
		t.Fatalf("refused with %s, want POINTER_ROLLBACK_REJECTED", code)
	}

	// The assertion that separates the two layers.
	if after := counter.artifactFetches(); len(after) != before {
		t.Errorf("the refused resolution fetched %d artifact(s) before refusing:\n%v\n"+
			"MKT-050's resolution-time check exists so a rollback is refused WITHOUT downloading — the "+
			"in-transaction check answers the same code, but only after the artifact has been retrieved and "+
			"verified", len(after)-before, after[before:])
	}

	// And nothing landed: the installed version is still the one that was there.
	pack, found, perr := st.GetPack(ctx, "acme/menu-board")
	if perr != nil || !found {
		t.Fatalf("GetPack after a refused rollback: found=%v err=%v", found, perr)
	}
	if pack.Version != "2.0.0" {
		t.Errorf("installed version after a refused rollback = %s, want the unchanged 2.0.0", pack.Version)
	}
}

// TestExplicitPinIsNotGovernedByTheRollbackRule is the exemption, and it is what
// stops the rule above from being read as "never install an older version".
//
// MKT-050 governs CHANNEL-POINTER resolution only. An explicit version pin is
// the operator's own stated choice, and MKT-044's reinstall of an archived
// version depends on it staying reachable — so a pin below the mark must still
// resolve. Testing only the refusal would leave an over-strict implementation
// passing, and it would break archived reinstall for every pack.
func TestExplicitPinIsNotGovernedByTheRollbackRule(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	reg := newRegistry(t, "fixture")
	src := reg.source()
	in, signer := marketInstaller(t, st, src)

	publishVersion(t, reg, signer, "1.0.0")
	publishVersion(t, reg, signer, "2.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("install 2.0.0 through the pointer: %v", err)
	}

	// An explicit pin below the mark: allowed, because the operator named it. The
	// channel is still supplied — MKT-060a forbids a host defaulting one — so
	// what distinguishes this from the case above is the VERSION being named,
	// not the reference being channel-less.
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community", Version: "1.0.0"}); err != nil {
		t.Fatalf("an explicit pin below the high-water mark was refused: %v — MKT-050 governs channel-pointer "+
			"resolution only, and MKT-044's archived reinstall depends on this staying reachable", err)
	}
	pack, found, err := st.GetPack(ctx, "acme/menu-board")
	if err != nil || !found {
		t.Fatalf("GetPack: found=%v err=%v", found, err)
	}
	if pack.Version != "1.0.0" {
		t.Errorf("installed version after an explicit pin = %s, want 1.0.0", pack.Version)
	}
}
