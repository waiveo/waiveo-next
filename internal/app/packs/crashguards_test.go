package packs_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/packsig"
)

// The crash group of the sweep recorded in issue #178.
//
// These four guards differ from the rest in what deleting them costs. The others
// lose a refusal, or blur a diagnosis. These four turn a MISCONFIGURATION into a
// nil dereference — so the pack subsystem does not answer wrongly, it takes the
// process down, and it does so on a path an operator reaches by getting a config
// field wrong rather than by doing anything unusual.
//
// Two of the four turned out to be unreachable rather than untested. That is
// recorded on each rather than quietly dropped, because "this guard cannot fire
// today" and "this guard is not tested" call for opposite responses: the first
// wants the invariant it rests on pinned, the second wants the guard driven.

// TestUpdatingAPackWhoseRegistryHasBeenRemovedFromConfigIsRefusedNotACrash.
//
// The reachable form of this took a second attempt, and the first attempt is
// worth recording because it made the guard look less important than it is.
//
// Installing DIRECTLY and then updating does not reach the nil marketplace: the
// record carries no trust channel, so a later check refuses with
// TRUST_CHANNEL_UNKNOWN first, and deleting the guard costs only a diagnosis.
// The case that reaches it is an operator REMOVING a registry source from
// configuration and then updating a pack that was installed from it — the record
// still names a trust channel, so nothing refuses earlier and the update walks
// into `in.market.resolve` on a nil *Market.
//
// That is an ordinary sequence: change the config, restart, click update on a
// pack that used to have a home. Without the guard it takes the process down.
func TestUpdatingAPackWhoseRegistryHasBeenRemovedFromConfigIsRefusedNotACrash(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	signer := newTestSigner(t)

	// Installed FROM a registry, so the record pins a trust channel.
	reg := newRegistry(t, "local-fixture")
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	reg.point("acme/menu-board", "community", "1.0.0")
	withMarket := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...),
		packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow }, reg.source())))
	if _, err := withMarket.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef: %v", err)
	}

	// The same store, now served by a deployment whose registry configuration is
	// gone — what an operator gets after editing config and restarting.
	noMarket := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...))

	_, err := noMarket.Update(ctx, "acme/menu-board")
	if err == nil {
		t.Fatal("Update with no marketplace configured reported success — there is no source to re-resolve against")
	}
	artifactCode(t, err, "MARKETPLACE_REF_UNRESOLVED")

	// The control: the same installer still refuses an UNINSTALLED pack with the
	// store's own not-found rather than this code, so the guard has not become a
	// blanket answer that hides every other failure on this path.
	if _, err := noMarket.Update(ctx, "acme/nothing-installed"); err == nil {
		t.Error("Update reported success for a pack that is not installed")
	} else {
		var aerr *packs.ArtifactError
		if errors.As(err, &aerr) && aerr.Code == "MARKETPLACE_REF_UNRESOLVED" {
			t.Error("an uninstalled pack was refused with the no-marketplace code — the nil-market check must not " +
				"sit above the not-found answer, or every update failure reads as a missing registry")
		}
	}

	// And the configuration that DOES have a registry still updates, so the
	// refusal above is about the missing marketplace and nothing else.
	if _, err := withMarket.Update(ctx, "acme/menu-board"); err != nil {
		t.Fatalf("the configured installer could not update the same pack: %v", err)
	}
}

// TestASourceWithNoTransportIsRefusedNotACrash.
//
// NewMarket keeps whatever Sources it is handed, filtering only the direct
// sentinel — it does not require a Fetcher. A Source is a config-shaped value
// (id, channel, index URL, transport), so one assembled with its transport
// unset is an ordinary configuration mistake.
//
// Without the guard, fetchIndex calls Fetch on a nil interface. A single
// misconfigured source therefore crashes every resolution — including the ones
// that would have been answered by the OTHER, correctly configured sources,
// since resolution walks them in order.
func TestASourceWithNoTransportIsRefusedNotACrash(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	signer := newTestSigner(t)

	broken := packs.Source{ID: "misconfigured", Channel: "marketplace/stable", IndexURL: "file:///index.json"}
	if broken.Fetcher != nil {
		t.Fatal("the fixture is meant to have no transport")
	}

	market := packs.NewMarket(func() int64 { return fixedNow }, broken)
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))

	_, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if err == nil {
		t.Fatal("a source with no transport resolved a pack")
	}
	artifactCode(t, err, "MARKETPLACE_REF_UNRESOLVED")

	// The control, and the point of the guard: a WORKING source beside the broken
	// one still resolves. Without this the test would pass against an
	// implementation that simply refused whenever any source was misconfigured,
	// which is the outage the guard exists to avoid rather than cause.
	reg := newRegistry(t, "local-fixture")
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	reg.point("acme/menu-board", "community", "1.0.0")

	mixed := packs.NewMarket(func() int64 { return fixedNow }, broken, reg.source())
	in2 := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(mixed))
	if _, err := in2.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("a working source beside a transport-less one did not resolve: %v — one misconfigured source must "+
			"not take the others down with it", err)
	}
}

// TestATransportThatFailsIsReportedWithoutLeakingHostDetail drives the path that
// asArtifactError's own guard sits on, and records what that experiment showed.
//
// The sweep flagged the `Unwrap` type assertion in asArtifactError: deleted, an
// error chain that ends in a plain error calls Unwrap through a nil interface
// and panics. The question a test has to answer is whether such a chain can
// REACH it, and the answer here is no — fetchIndex converts a transport failure
// into an ArtifactError with a fresh message rather than wrapping it, so the
// underlying error never travels. That is also why the guard survived: nothing
// in production hands it a chain it cannot walk.
//
// The conversion is deliberate and worth pinning on its own terms: the message
// a fetch failure produces names host paths and OS errors, and the client is
// told only that the source did not answer.
func TestATransportThatFailsIsReportedWithoutLeakingHostDetail(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	signer := newTestSigner(t)

	const secret = "/var/lib/waiveo/private/index.json: permission denied"
	market := packs.NewMarket(func() int64 { return fixedNow }, packs.Source{
		ID:       "failing",
		Channel:  "marketplace/stable",
		IndexURL: "file:///index.json",
		Fetcher:  fetcherFunc(func(context.Context, string) ([]byte, error) { return nil, errors.New(secret) }),
	})
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...), packs.WithMarketplace(market))

	_, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if err == nil {
		t.Fatal("a source whose transport always fails resolved a pack")
	}
	artifactCode(t, err, "MARKETPLACE_REF_UNRESOLVED")

	var aerr *packs.ArtifactError
	if errors.As(err, &aerr) && strings.Contains(aerr.Message, secret) {
		t.Errorf("the client-facing refusal carries the transport's own error %q — that message names host paths "+
			"and OS failures, which is operator detail", aerr.Message)
	}
}

// fetcherFunc adapts a function to the Fetcher interface.
type fetcherFunc func(ctx context.Context, rawURL string) ([]byte, error)

func (f fetcherFunc) Fetch(ctx context.Context, rawURL string) ([]byte, error) { return f(ctx, rawURL) }

// TestEveryVerifyBundleRefusalIsATypedVerifyError pins the invariant the
// signature path's own type assertion rests on.
//
// verifyArtifactSignature asserts VerifyBundle's error to *packsig.VerifyError
// and reads .Reason to choose between PACK_UNSIGNED, PACK_SIGNER_UNTRUSTED and
// PACK_SIGNATURE_INVALID. The guard on that assertion survives the sweep, and
// the reason is the same one the reader's over-read check has: it cannot fire,
// because every error VerifyBundle returns today IS that type.
//
// That makes the guard correct and the invariant load-bearing — and the
// invariant is the part nothing was watching. A plain error added anywhere in
// the verify path would, without the guard, be dereferenced as a *VerifyError in
// the SIGNATURE path; with the guard it silently becomes PACK_SIGNATURE_INVALID,
// which would mislabel an unsigned or untrusted artifact as a bad signature.
// This test fails at the moment such an error appears, rather than either
// consequence being discovered later.
func TestEveryVerifyBundleRefusalIsATypedVerifyError(t *testing.T) {
	anchors := newTestSigner(t).anchorsFor(fixtureNamespaces...)

	for _, tc := range []struct {
		name  string
		files map[string][]byte
	}{
		{"no envelope at all", map[string][]byte{"manifest.json": []byte(`{"id":"acme/menu-board"}`)}},
		{"an envelope that is not JSON", withEnvelope(`not json`)},
		{"an envelope that is not an object", withEnvelope(`["nope"]`)},
		{"an envelope declaring a member twice", withEnvelope(`{"format":"a","format":"b"}`)},
		{"an envelope of the wrong format", withEnvelope(`{"format":"not-ours"}`)},
		{"an empty envelope object", withEnvelope(`{}`)},
		{"a truncated envelope", withEnvelope(`{"format":`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := packsig.VerifyBundle(tc.files, anchors)
			if err == nil {
				t.Fatal("verification succeeded")
			}
			var verr *packsig.VerifyError
			if !errors.As(err, &verr) {
				t.Fatalf("VerifyBundle returned %v (%T), which is not a *packsig.VerifyError — the install path "+
					"reads .Reason off this error to tell an UNSIGNED artifact from an UNTRUSTED signer from a "+
					"bad signature, and an untyped error collapses all three into 'invalid signature'", err, err)
			}
			if verr.Reason == "" {
				t.Error("a typed refusal carried no reason, so the install path has nothing to map")
			}
		})
	}
}

// withEnvelope builds a minimal file set carrying the given envelope bytes.
func withEnvelope(body string) map[string][]byte {
	return map[string][]byte{
		"manifest.json":      []byte(`{"id":"acme/menu-board"}`),
		packsig.EnvelopeName: []byte(body),
	}
}
