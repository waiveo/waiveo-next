package automationhost

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// TestEndToEndFromFeederSnapshot is the Task-4 end-to-end proof that the four
// relay subsystems run as one binary's worth of components: the feeder BUILDS +
// SIGNS a desired-state snapshot carrying its edge_rules section (REL-062), the
// Host loads that section's rule into the engine, and a synthetic screen-on
// observation drives the fired rule end to end — compile → engine → device-plane
// dispatch → durable automation.run. This is the same signed rule JSON the
// running relay pulls; here it is taken straight off the feeder's built snapshot.
func TestEndToEndFromFeederSnapshot(t *testing.T) {
	// The feeder builds + signs a snapshot; its edge_rules section carries the
	// Wave-1 first-automation demo rule (REL-062).
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	snap, err := snapshot.Build([]byte("demo-image-bytes"), contenturl.Signer{Base: "https://relay.example"}, id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	edgeRules := snap.Sections.EdgeRules.Rules
	if len(edgeRules) == 0 {
		t.Fatalf("built snapshot carried no edge_rules to drive the e2e")
	}

	// Boot the Host over a fresh operational store and load the feeder's rule.
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	controller := &recordController{}
	host, store := newTestHost(t, dbPath, controller)
	if err := host.ApplyEdgeRules(edgeRules, int(snap.Generation)); err != nil {
		t.Fatalf("ApplyEdgeRules from feeder snapshot: %v", err)
	}
	if host.EdgeRuleCount() != 1 {
		t.Fatalf("loaded %d edge rules from the snapshot, want 1", host.EdgeRuleCount())
	}

	// Drive a synthetic screen-on observation end to end through the Host's Run
	// loop: baseline "off" then the "off→on" transition that fires the rule.
	reg := registry.FixtureRegistry{}
	src := NewSyntheticSource(
		state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "off")),
		state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "on")),
	)
	if err := host.Run(context.Background(), src); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Fire → dispatch: the physical device received exactly one launch.
	if got := controller.commands(); len(got) != 1 || got[0] != testScreenEntity+"/launch" {
		t.Fatalf("device saw %v, want exactly one launch", got)
	}

	// Fire → durable automation.run: one automation.run entry is durably buffered
	// in the operational store.
	assertOneDurableAutomationRun(t, store)
}
