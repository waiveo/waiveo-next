package packs_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// MKT-050a's grammar check guards the version ORDER, and it sits at two points on
// the resolution path: the channel pointer's value, and the index entry's own
// version. Both survived the sweep in issue #178, and they turned out to be two
// different things.
//
// The POINTER check is the real one, and it is the only guard on its path: a
// pointer is consulted precisely when no version was pinned, so nothing upstream
// has looked at its value.
//
// The ENTRY check cannot fire. selectEntry matches by exact string equality on
// the version it is handed, and that version has exactly two origins — a pinned
// Ref.Version, refused by Ref.validate before any index is fetched, and a
// pointer, refused by the check above. So the entry's version always equals a
// string that already passed the grammar. It is a backstop, and what this file
// pins for it is the pair of upstream refusals it rests on.
//
// The pointer assertion is on the MESSAGE rather than the code, because several
// refusals on this path carry MARKETPLACE_REF_UNRESOLVED and a code assertion
// cannot tell one from another — the same trap that let the locale pair look
// covered.
//
// Worth stating what none of this catches, since it is the subject of the gated
// issue #173: `1.0.05` satisfies this grammar and compares EQUAL to `1.0.5`, so a
// pointer moved between those two spellings passes every check here. These guard
// the boundary between "a version" and "not a version", not the injectivity of
// the mapping inside it.

// TestAChannelPointerNamingANonVersionDoesNotResolve.
//
// MKT-047 makes a pointer's value a LOOKUP KEY rather than a trust decision, and
// MKT-050a adds that a value outside the grammar has no position in the version
// order. Together that is why a bad pointer is UNRESOLVABLE rather than compared
// some other way: there is no other way to compare it.
//
// Without the check the pointer's value flows on as the version to select. What
// happens next depends on the index — an entry at that literal string resolves,
// or the lookup misses — and both outcomes are worse than a refusal here,
// because MKT-050's no-backward-walk rule is evaluated against a value the
// ordering cannot place.
func TestAChannelPointerNamingANonVersionDoesNotResolve(t *testing.T) {
	for _, pointer := range []string{"1.0", "latest", "v1.0.0", "1.0.0-rc1", ""} {
		t.Run("pointer="+pointer, func(t *testing.T) {
			st := openStore(t)
			signer := newTestSigner(t)
			reg := newRegistry(t, "local-fixture")

			// A real, correctly published artifact — so nothing else in the index
			// is wrong and the pointer is the only fault.
			art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
			reg.publish("acme/menu-board", "1.0.0", art, nil)
			reg.point("acme/menu-board", "community", pointer)

			in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...),
				packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow }, reg.source())))

			_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
			if err == nil {
				t.Fatalf("a channel pointer naming %q resolved", pointer)
			}
			artifactCode(t, err, "MARKETPLACE_REF_UNRESOLVED")

			var aerr *packs.ArtifactError
			if !errors.As(err, &aerr) {
				t.Fatalf("error = %v (%T)", err, err)
			}
			if !strings.Contains(aerr.Message, "channel pointer") {
				t.Errorf("a bad POINTER was refused as %q — the refusal must name the pointer as the fault, since "+
					"the index and its artifact are correct and the only thing wrong is what the channel points at",
					aerr.Message)
			}
		})
	}

	// The control: a pointer naming a real version still resolves. Without it,
	// every case above is satisfied by a pipeline that resolves nothing.
	st := openStore(t)
	signer := newTestSigner(t)
	reg := newRegistry(t, "local-fixture")
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	reg.point("acme/menu-board", "community", "1.0.0")
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...),
		packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow }, reg.source())))
	if _, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("a pointer naming a real version did not resolve: %v", err)
	}
}

// TestEveryVersionReachingTheIndexHasAlreadyPassedTheGrammar pins the invariant
// that makes the entry-side check unreachable, and records why the check on the
// index entry's own version is not the gap it looked like.
//
// selectEntry matches an entry by EXACT string equality on the version it is
// given, so the entry it returns always carries precisely that string. The
// version it is given has exactly two origins, and both are already validated:
// a pinned Ref.Version, refused by Ref.validate as MARKETPLACE_REF_INVALID
// before any index is fetched, and a channel pointer, refused by the check the
// test above drives. So `!ValidVersion(entry.Version)` cannot be true.
//
// My first version of this test requested a non-version explicitly and expected
// the entry-side refusal; it got MARKETPLACE_REF_INVALID from Ref.validate
// instead, which is how the unreachability surfaced. Three of this sweep's
// guards have now turned out this way, and the pattern is the same each time: a
// guard that cannot fire because something upstream already refuses its input.
//
// So the entry-side check stays as a backstop, and what is pinned here is the
// pair of upstream refusals it depends on. If either is ever relaxed, this test
// fails rather than the backstop quietly becoming the only thing standing.
func TestEveryVersionReachingTheIndexHasAlreadyPassedTheGrammar(t *testing.T) {
	st := openStore(t)
	signer := newTestSigner(t)
	reg := newRegistry(t, "local-fixture")
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	reg.point("acme/menu-board", "community", "1.0.0")
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...),
		packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow }, reg.source())))

	// Origin one: a version pinned on the reference. Refused before any index is
	// fetched, so a malformed version never becomes a lookup key.
	for _, version := range []string{"1.0", "latest", "v1.0.0", "1.0.0.0", "1.0.0-rc1"} {
		_, err := in.InstallRef(context.Background(),
			packs.Ref{PackID: "acme/menu-board", Version: version, TrustChannel: "community"})
		if err == nil {
			t.Errorf("a reference pinning version %q resolved", version)
			continue
		}
		artifactCode(t, err, "MARKETPLACE_REF_INVALID")
	}

	// Origin two is the channel pointer, covered by the test above. Together they
	// are every way a version reaches the index.

	// The control: a pinned version that IS in the grammar resolves, so the
	// refusals above are about the grammar and not about pinning.
	if _, err := in.InstallRef(context.Background(),
		packs.Ref{PackID: "acme/menu-board", Version: "1.0.0", TrustChannel: "community"}); err != nil {
		t.Fatalf("a reference pinning a real version did not resolve: %v", err)
	}
}
