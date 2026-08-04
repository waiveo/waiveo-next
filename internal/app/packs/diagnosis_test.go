package packs_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/packs"
)

// The last group from the sweep in issue #178, and the honest summary of it is
// that these guards cost no refusal at all.
//
// Deleting either one still refuses the request, with the same
// MARKETPLACE_REF_UNRESOLVED code — what changes is the sentence an operator
// reads, and in both cases the sentence they would get instead describes a
// situation that is not theirs. That is worth a test and is not worth calling a
// security finding, so it is filed here rather than dressed up as one.

// TestADeploymentWithNoRegistryConfiguredSaysSo.
//
// With the guard gone, resolution iterates an empty source list, no source is
// tried, and the fall-through refusal says "no configured registry source
// resolves acme/menu-board on the community trust channel" — which reads as
// though sources were consulted and none carried the pack. The operator's actual
// situation is that they have configured none.
//
// Those two send someone to different places: one to the registry to ask why the
// pack is missing, the other to their own configuration.
func TestADeploymentWithNoRegistryConfiguredSaysSo(t *testing.T) {
	st := openStore(t)
	signer := newTestSigner(t)

	// A marketplace with no sources at all — what a deployment that has not
	// configured a registry has.
	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...),
		packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow })))

	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if err == nil {
		t.Fatal("a deployment with no registry sources resolved a pack")
	}
	artifactCode(t, err, "MARKETPLACE_REF_UNRESOLVED")

	var aerr *packs.ArtifactError
	if !errors.As(err, &aerr) {
		t.Fatalf("error = %v (%T)", err, err)
	}
	if !strings.Contains(aerr.Message, "no registry sources configured") {
		t.Errorf("a deployment with NO sources was told %q — that sentence describes sources having been consulted "+
			"and come up empty, which sends an operator to the registry rather than to their own configuration",
			aerr.Message)
	}

	// The control: with a source configured, a pack the registry does not carry
	// gets the OTHER sentence, so the two remain distinguishable rather than one
	// having swallowed the other.
	reg := newRegistry(t, "local-fixture")
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, nil)
	reg.point("acme/menu-board", "community", "1.0.0")
	configured := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...),
		packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow }, reg.source())))

	_, err = configured.InstallRef(context.Background(), packs.Ref{PackID: "acme/nothing-here", TrustChannel: "community"})
	if err == nil {
		t.Fatal("a pack no source carries resolved")
	}
	if errors.As(err, &aerr) && strings.Contains(aerr.Message, "no registry sources configured") {
		t.Errorf("a CONFIGURED deployment was told it has no registry sources: %q", aerr.Message)
	}

	// And the same configured installer still resolves what the registry does
	// carry, so neither refusal above is a pipeline that refuses everything.
	if _, err := configured.InstallRef(context.Background(),
		packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("a configured deployment could not install a published pack: %v", err)
	}
}

// TestAChannelWithNoPointerIsNotReportedAsPointingAtNothing.
//
// This is the sharper of the two, because the message the guard's absence
// produces is not merely less useful — it is untrue.
//
// pointerVersion returns ("", false) when the source publishes no pointer for
// this (pack, channel). Without the guard that empty string flows on as the
// version, and the grammar check below refuses it — reporting that "the community
// channel pointer for acme/menu-board names "", which is not a three-component
// MAJOR.MINOR.PATCH version".
//
// There is no pointer. The refusal describes a malformed one, which points an
// index author at a value to correct rather than at a channel to publish.
func TestAChannelWithNoPointerIsNotReportedAsPointingAtNothing(t *testing.T) {
	st := openStore(t)
	signer := newTestSigner(t)
	reg := newRegistry(t, "local-fixture")

	// The artifact is published and the index is otherwise correct — the only
	// thing absent is a pointer for this channel.
	art := signer.sign(t, basePackZip(t, baseManifest()), "acme/menu-board", "1.0.0")
	reg.publish("acme/menu-board", "1.0.0", art, nil)

	in := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...),
		packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow }, reg.source())))

	_, err := in.InstallRef(context.Background(), packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"})
	if err == nil {
		t.Fatal("a channel with no pointer resolved")
	}
	artifactCode(t, err, "MARKETPLACE_REF_UNRESOLVED")

	var aerr *packs.ArtifactError
	if !errors.As(err, &aerr) {
		t.Fatalf("error = %v (%T)", err, err)
	}
	if !strings.Contains(aerr.Message, "publishes no") {
		t.Errorf("a MISSING pointer was reported as %q — the source publishes no pointer for this channel at all, "+
			"so a message describing the pointer's value sends an index author to correct a value that does not "+
			"exist instead of publishing the channel", aerr.Message)
	}
	if strings.Contains(aerr.Message, `names ""`) {
		t.Errorf("the refusal claims the pointer NAMES the empty string: %q", aerr.Message)
	}

	// The control: the same index with the pointer published resolves, so the
	// refusal is about the missing pointer and nothing else in the fixture.
	reg.point("acme/menu-board", "community", "1.0.0")
	in2 := packs.NewInstaller(st, signer.anchorsFor(fixtureNamespaces...),
		packs.WithMarketplace(packs.NewMarket(func() int64 { return fixedNow }, reg.source())))
	if _, err := in2.InstallRef(context.Background(),
		packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("the same index with the pointer published did not resolve: %v", err)
	}
}
